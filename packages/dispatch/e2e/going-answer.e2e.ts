import { GOING_ANSWER_WORD_MAX } from "@legion/contracts";

import { createIssue, createProject, getGoingCapability, search } from "./api";
import {
  chatCalls,
  decisionCalls,
  decisionRecord,
  modelsLane,
  scriptChat,
} from "./fake-models-helpers";
import {
  answerPanel,
  answerText,
  askAndWait,
  bar,
  countAskedRuns,
  openBarOver,
  plainWords,
  prose,
  searchBox,
  shownWords,
} from "./going-panel";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

// Check c1 of dispatch://INFRA-1588/artifact/verification-md, serving R1: with the fake writing
// model, the bar's answer is short, linked to what it read, and absent when there is none, in a
// panel of its own beside the hits. Each row scripts what the model writes and reads what the
// panel draws of it, after the server's filter: the cut, the dropped link, the none frame, the
// end frame's count of items. No model is called on a keystroke, which the last row counts on
// the fakes themselves. Every row runs in the models lane alone (`bun run e2e:models`).

/** The words a person asks, which the seeded issue's title answers. */
const question = "quasar sightings log";
const title = "Quasar sightings log";

/** The seeded issue and the href search gives its hit, which is the link the model is given. */
async function seedIssue(): Promise<{ readonly key: string; readonly href: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title });
  const hit = ((await search(question)).results ?? []).find(
    (result) => result.owner.kind === "issue" && result.owner.key === issue.key
  );
  if (hit === undefined) {
    throw new Error(`search for "${question}" does not find ${issue.key}`);
  }
  return { href: hit.href, key: issue.key };
}

/** Records, in the page, the order in which the hits, the answer's first text and the answer's
 *  end reached the screen, and whether text was on the screen while the answer still streamed. */
async function recordOrder(page: Page): Promise<void> {
  await page.evaluate(() => {
    const seen: string[] = [];
    Reflect.set(window, "__goingOrder", seen);
    const note = (what: string) => {
      if (!seen.includes(what)) seen.push(what);
    };
    const look = () => {
      if (document.querySelector('[role="option"][id^="search-option-issue-"]') !== null) {
        note("hits");
      }
      const panel = document.querySelector('[data-testid="going-answer"]');
      const text = document.querySelector('[data-testid="going-answer-text"]')?.textContent ?? "";
      if (text !== "") {
        note("text");
        if (panel?.getAttribute("data-state") === "streaming") note("text-while-streaming");
      }
      if (panel?.getAttribute("data-state") === "ended") note("ended");
    };
    new MutationObserver(look).observe(document.body, {
      attributes: true,
      characterData: true,
      childList: true,
      subtree: true,
    });
  });
}

function recordedOrder(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const seen: unknown = Reflect.get(window, "__goingOrder");
    return Array.isArray(seen) ? seen.map(String) : [];
  });
}

// SLOW(>=2s): a seeded issue, a decision and an answer streamed slowly by the fake model.
test("the stack answers and decides through the fakes, and a question a seeded issue answers links it beside hits painted first, streaming before it ends", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  // t8's acceptance: this stack answers and decides through the fakes, and their counts read
  // back from their fixture routes, nothing called yet.
  const capability = await getGoingCapability();
  expect(capability.answer.configured).toBe(true);
  expect(capability.decision.configured).toBe(true);
  expect(capability.decision.model).toBe(decisionRecord().model);
  expect((await chatCalls()).calls).toBe(0);
  expect((await decisionCalls()).calls).toBe(0);

  const issue = await seedIssue();
  await scriptChat({
    default: prose(
      `The sightings are kept in [${title}](${issue.href}). ${plainWords("sighting", 40)}`,
      { chunk: 6, delayMs: 120 }
    ),
  });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    await recordOrder(page);
    await askAndWait(page, question);

    // The hits were on the screen before the answer's first words, and the answer's words were
    // on the screen while it was still streaming, before its end.
    const order = await recordedOrder(page);
    expect(order.indexOf("hits")).toBeGreaterThanOrEqual(0);
    expect(order.indexOf("hits")).toBeLessThan(order.indexOf("text"));
    expect(order).toContain("text-while-streaming");
    expect(order.indexOf("text-while-streaming")).toBeLessThan(order.indexOf("ended"));

    // In its own panel, beside the hits, linking the issue that answers it.
    await expect(bar(page).getByRole("listbox")).toBeVisible();
    await expect(bar(page).locator(`[id^="search-option-issue-${issue.key}"]`)).toBeVisible();
    const items = answerPanel(page).getByTestId("going-answer-item");
    await expect(items).toHaveCount(1);
    await expect(items).toHaveAttribute("href", issue.href);
    await expect(answerText(page)).toContainText("The sightings are kept in");
    expect((await decisionCalls()).calls).toBe(1);
    expect((await chatCalls()).calls).toBeGreaterThanOrEqual(1);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a decision and a 300-word answer.
test("a 300-word reply is cut to 120 words and the bar says it was cut", async ({ browser }) => {
  modelsLane();
  test.setTimeout(90_000);
  await seedIssue();
  await scriptChat({ default: prose(plainWords("alpha", 300)) });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    await askAndWait(page, question);

    await expect(page.getByTestId("going-answer-cut")).toHaveText(
      `answer cut to ${GOING_ANSWER_WORD_MAX} words`
    );
    const words = await shownWords(page);
    expect(words).toHaveLength(GOING_ANSWER_WORD_MAX);
    expect(words.at(-1)).toBe(`alpha${GOING_ANSWER_WORD_MAX - 1}`);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a decision and an answer.
test("a link to an item the model was not given is dropped and the bar says the answer changed", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  await seedIssue();
  await scriptChat({
    default: prose("The plan is in [the hidden plan](/issues/ZZZ-404) and nowhere else."),
  });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    await askAndWait(page, question);

    await expect(page.getByTestId("going-answer-changed")).toContainText("the answer changed");
    // The link's text stays as text; the link itself, and any item for it, is gone.
    await expect(answerText(page)).toContainText("the hidden plan");
    await expect(answerText(page).locator("a")).toHaveCount(0);
    await expect(answerPanel(page).locator('a[href*="ZZZ-404"]')).toHaveCount(0);
    await expect(answerPanel(page).getByTestId("going-answer-item")).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a decision and an answer.
test("a reply with no answer text shows no answer: the bar says it found none and shows the hits", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  const issue = await seedIssue();
  await scriptChat({ default: prose("") });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    await askAndWait(page, question);

    await expect(page.getByTestId("going-answer-none")).toHaveText("no answer found");
    await expect(answerText(page)).toHaveCount(0);
    await expect(page.getByTestId("going-answer-no-source")).toHaveCount(0);
    await expect(bar(page).locator(`[id^="search-option-issue-${issue.key}"]`)).toBeVisible();
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a decision and an answer.
test("an answer that kept no link is shown, saying it rests on no source", async ({ browser }) => {
  modelsLane();
  test.setTimeout(90_000);
  await seedIssue();
  await scriptChat({ default: prose("The sightings are written down in the log every night.") });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    await askAndWait(page, question);

    await expect(answerText(page)).toContainText("written down in the log every night");
    await expect(page.getByTestId("going-answer-no-source")).toHaveText("rests on no source");
    await expect(answerPanel(page).getByTestId("going-answer-item")).toHaveCount(0);
    await expect(page.getByTestId("going-answer-none")).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a question typed a key at a time, a count of both fakes' calls after every key,
// and the one answer the pause asks for.
test("no model call is made on a keystroke: only the pause after the last one asks", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(90_000);
  await seedIssue();
  await scriptChat({ default: prose("The log holds them.") });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await openBarOver(page, "/projects/CORE/issues");
    const asked = countAskedRuns(page);

    // Each key lands inside the pause the key before it started, and after each one neither fake
    // has been called and the bar has asked nothing.
    for (const key of question) {
      await searchBox(page).press(key === " " ? "Space" : key);
      expect(asked.words).toEqual([]);
      expect((await chatCalls()).calls).toBe(0);
      expect((await decisionCalls()).calls).toBe(0);
    }

    // The pause after the last key asks once, and that one question is what the fakes answer.
    await expect(answerPanel(page)).toHaveAttribute("data-state", "ended", { timeout: 40_000 });
    expect(asked.words).toEqual([question]);
    expect((await decisionCalls()).calls).toBe(1);
    expect((await chatCalls()).calls).toBeGreaterThanOrEqual(1);
  } finally {
    await context.close();
  }
});
