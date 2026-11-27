import { createIssue, createProject } from "./api";
import {
  callTool,
  decided,
  modelsLane,
  prose,
  runCalls,
  say,
  scriptChat,
  scriptDecision,
} from "./fake-models-helpers";
import {
  ANSWER_ENDS_MS,
  answerPanel,
  askTheAgent,
  bar,
  bringOverThePage,
  closeBar,
  conversationBox,
  expectHandedToThePanel,
  lineOnTop,
  newestRun,
  openBarOver,
  RUN_STEPS_MS,
  searchBox,
  sidePanel,
} from "./going-locators";
import { countStartedRuns } from "./going-panel";
import { expect, type Page, test } from "./test";
import { withAlice } from "./users";

// Check c1 of dispatch://INFRA-1588/artifact/verification-md (spec R1): Ctrl-K opens the bar over
// the page and Esc closes it; the bar draws the short answer as a card above the hits, and Open in
// panel on that card, and Enter on the line Ask, are one action - one run, with the words, level
// and context the bar holds now, the bar closed and the cursor in the box of the panel at the
// side, which leaves the page usable and stays open on every navigation; a card for words typed
// past has no such button, so an old preview cannot submit an old query; a link in the panel opens
// its item in the page and the next request names that item; Ctrl-K brings the conversation back
// over the page and Ctrl-Shift-K keeps it at the side; and the header's button widens the panel
// to the app's content area, with the navigation beside it, and docks it back, with the same run
// and the unsent words. Do keeps its conversation in the bar. The models lane runs it
// (`bun run e2e:models`), and its Firefox row the `firefox` project
// (`bun run e2e:models:firefox`). Negative controls: a panel that ignores navigation sends the
// next request with no issue; a bar that opens its own full Ask conversation fails the handoff
// rows, which find the bar still open.

async function seedList(): Promise<{ readonly first: string; readonly second: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const first = await createIssue({ project: "CORE", title: "Chart the comet's tail" });
  const second = await createIssue({ project: "CORE", title: "Count the moons of Saturn" });
  return { first: first.key, second: second.key };
}

/** The panel at the side shows the conversation the bar handed over: the run of `words`. */
async function expectAtTheSide(page: Page, words: string): Promise<void> {
  await expect(bar(page)).toHaveCount(0);
  await expect(sidePanel(page)).toBeVisible();
  await expect(sidePanel(page).getByTestId("going-run-words").first()).toHaveText(words);
}

/** The list stays usable beside the panel: a click on the issue `key` of it opens that issue. */
async function openFromTheList(page: Page, key: string): Promise<void> {
  await page
    .getByRole("link", { name: new RegExp(key) })
    .first()
    .click();
  await expect(page).toHaveURL(new RegExp(`/issues/${key}`));
  await expect(sidePanel(page)).toBeVisible();
}

// SLOW(>=2s): a seeded issue and one open and close of the bar.
test("opens over the issue page", async ({ browser }) => {
  modelsLane();
  const { first } = await seedList();
  await withAlice(browser, async (page) => {
    await openBarOver(page, `/issues/${first}`);
    await expect(bar(page)).toBeVisible();
    await closeBar(page);
  });
});

// SLOW(>=2s): one open and close of the bar over the Inbox.
test("opens over the Inbox", async ({ browser }) => {
  modelsLane();
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/");
    await expect(bar(page)).toBeVisible();
    await page.keyboard.press("Control+k");
    await expect(bar(page)).toHaveCount(0);
  });
});

// SLOW(>=2s): the pause's short answer, Tab from the box to the card's button and Enter, one run.
test("Open in panel on the card, pressed from the keyboard, starts one run and hands it to the panel beside the usable page", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  const { second } = await seedList();
  const asked = "what is on this list";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [{ turns: [say("Two issues about the sky.")], words: asked }],
  });
  await withAlice(browser, async (page) => {
    const started = countStartedRuns(page);
    await openBarOver(page, "/projects/CORE/issues");
    await searchBox(page).fill(asked);
    // The card is above the hits and outside the list, and receiving its answer started no run
    // and opened no panel.
    await expect(answerPanel(page)).toHaveAttribute("data-state", "ended", {
      timeout: ANSWER_ENDS_MS,
    });
    await expect(sidePanel(page)).toHaveCount(0);
    expect(started.bodies).toHaveLength(0);
    const open = answerPanel(page).getByRole("button", { name: "Open in panel" });
    await expect(
      bar(page).getByRole("option").getByRole("button", { name: "Open in panel" })
    ).toHaveCount(0);

    // The person reaches the button from where they are, the bar's box, with Tab: the box's own
    // Filters button, then the card under the box, whose answer cites nothing, so its one control
    // is Open in panel.
    await expect(searchBox(page)).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(bar(page).getByRole("button", { name: "Filters" })).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(open).toBeFocused();
    await page.keyboard.press("Enter");
    await expectHandedToThePanel(page);
    await expectAtTheSide(page, asked);
    await expect(newestRun(page).getByTestId("going-run-answer")).toHaveText(
      "Two issues about the sky.",
      { timeout: RUN_STEPS_MS }
    );
    // Exactly one new run, of the words in the bar, the level it was set to and the page it was
    // opened over, as the line Ask.
    expect(started.bodies).toHaveLength(1);
    expect(started.bodies[0]).toMatchObject({
      context: { path: "/projects/CORE/issues", project: "CORE" },
      level: "standard",
      line: "ask",
      words: asked,
    });
    expect(started.bodies[0]).not.toHaveProperty("continues");
    await expect(sidePanel(page).getByTestId("going-run")).toHaveCount(1);
    await openFromTheList(page, second);
  });
});

// SLOW(>=2s): a run and the panel it is handed to, then a click on the page.
test("Enter on the line Ask hands the run to the panel the same way, and a click on an issue of the list is the next request's context", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  const { second } = await seedList();
  const asked = "what is on this list";
  const next = "what does this one need";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      { turns: [say("Two issues about the sky.")], words: asked },
      { turns: [say("It needs a telescope.")], words: next },
    ],
  });
  await withAlice(browser, async (page) => {
    const started = countStartedRuns(page);
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, asked);
    await expectHandedToThePanel(page);
    await expectAtTheSide(page, asked);
    expect(started.bodies).toHaveLength(1);
    expect(started.bodies[0]).toMatchObject({ line: "ask", words: asked });
    await expect(newestRun(page).getByTestId("going-run-answer")).toHaveText(
      "Two issues about the sky.",
      { timeout: RUN_STEPS_MS }
    );

    // The list stays usable beside the panel: a click on its issue opens it, and names it.
    await openFromTheList(page, second);
    const box = conversationBox(page);
    await box.fill(next);
    await box.press("Enter");
    await expect(sidePanel(page).getByText("It needs a telescope.")).toBeVisible({
      timeout: RUN_STEPS_MS,
    });
    const [call] = await runCalls(next);
    expect(call?.context).toMatchObject({ issue: second });
  });
});

// SLOW(>=2s): a card held stale by a held request, then released.
test("a card for words typed past offers no Open in panel, and the card of the words now submits those words", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  await seedList();
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [{ turns: [say("The quasars are catalogued.")], words: "quasars" }],
  });
  await withAlice(browser, async (page) => {
    const started = countStartedRuns(page);
    const { promise: held, resolve: release } = Promise.withResolvers<void>();
    // The pause's request for the longer words is held, so the card of the shorter ones stays
    // drawn as the earlier words' answer for as long as the row looks at it.
    await page.route("**/api/v1/going/requests", async (route) => {
      const body: unknown = route.request().postDataJSON();
      const words = body !== null && typeof body === "object" && "words" in body ? body.words : "";
      if (route.request().method() === "POST" && words === "quasars") await held;
      await route.continue();
    });
    await openBarOver(page, "/projects/CORE/issues");
    await searchBox(page).fill("quasar");
    await expect(answerPanel(page)).toHaveAttribute("data-state", "ended", {
      timeout: ANSWER_ENDS_MS,
    });
    await expect(answerPanel(page).getByRole("button", { name: "Open in panel" })).toBeVisible();

    await searchBox(page).press("s");
    await expect(answerPanel(page)).toHaveAttribute("data-state", "stale");
    await expect(bar(page).getByRole("button", { name: "Open in panel" })).toHaveCount(0);
    // Nothing of the old words started a run, and the card started none by being passed over.
    expect(started.bodies).toHaveLength(0);

    release();
    await expect(answerPanel(page)).toHaveAttribute("data-state", "ended", {
      timeout: ANSWER_ENDS_MS,
    });
    await answerPanel(page).getByRole("button", { name: "Open in panel" }).click();
    await expectHandedToThePanel(page);
    await expectAtTheSide(page, "quasars");
    expect(started.bodies).toHaveLength(1);
    expect(started.bodies[0]).toMatchObject({ line: "ask", words: "quasars" });
    expect(await runCalls("quasar")).toHaveLength(0);
  });
});

// SLOW(>=2s): a run handed to the panel, Ctrl-K over the page, and Ctrl-Shift-K at the side.
test("Ctrl-K brings the conversation in the panel back over the page, and Ctrl-Shift-K keeps it at the side", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  await seedList();
  const asked = "keep me company";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [{ turns: [say("Here beside you.")], words: asked }],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, asked);
    await expectHandedToThePanel(page);
    await expect(sidePanel(page).getByText("Here beside you.")).toBeVisible({
      timeout: RUN_STEPS_MS,
    });

    await bringOverThePage(page);
    await expect(sidePanel(page)).toHaveCount(0);
    await expect(bar(page).getByTestId("going-run-words").first()).toHaveText(asked);
    await conversationBox(page).press("Control+Shift+K");
    await expectAtTheSide(page, asked);
  });
});

// SLOW(>=2s): an answer that cites an issue, a click on its link in the panel, and a question.
test("a link in the panel's answer opens its item in the page, the panel keeps the conversation, and the next request names that item", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  const { first, second } = await seedList();
  const asked = "where is the moons work";
  const next = "who holds it";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [
      {
        turns: [
          callTool("dispatch_read", { issue: second }),
          say(`The moons are counted in [${second}](/issues/${second}).`),
        ],
        words: asked,
      },
      { turns: [say("Nobody yet.")], words: next },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, `/issues/${first}`);
    await askTheAgent(page, asked);
    await expectHandedToThePanel(page);
    const answer = sidePanel(page).getByTestId("going-run-answer");
    await expect(answer.getByTestId("item-chip")).toContainText(second, { timeout: RUN_STEPS_MS });
    await expect(newestRun(page)).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });

    await answer.getByRole("link", { name: second }).click();
    await expect(page).toHaveURL(new RegExp(`/issues/${second}`));
    // The conversation stays: the same run, its answer, and the panel at the side.
    await expect(sidePanel(page)).toBeVisible();
    await expectAtTheSide(page, asked);
    await expect(answer).toContainText("The moons are counted in");
    await expect(sidePanel(page).getByTestId("going-box-chip")).toHaveText(second);

    await conversationBox(page).fill(next);
    await conversationBox(page).press("Enter");
    await expect(sidePanel(page).getByText("Nobody yet.")).toBeVisible({ timeout: RUN_STEPS_MS });
    const [call] = await runCalls(next);
    expect(call?.context).toMatchObject({ issue: second, path: `/issues/${second}` });
  });
});

// SLOW(>=2s): a run, the panel at the side, and two navigations.
test("the panel stays open on navigation", async ({ browser }) => {
  modelsLane();
  test.setTimeout(90_000);
  await seedList();
  const asked = "keep me company";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [{ turns: [say("Here beside you.")], words: asked }],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, asked);
    await expectHandedToThePanel(page);
    await expectAtTheSide(page, asked);
    // A navigation inside the page, by the rail's own link, and Back: neither closes the panel.
    await page
      .getByRole("link", { name: /^Inbox/ })
      .first()
      .click();
    await expect(page).toHaveURL(/\/$/);
    await expect(sidePanel(page)).toBeVisible();
    await page.goBack();
    await expect(page).toHaveURL(/\/projects\/CORE\/issues/);
    await expect(sidePanel(page)).toBeVisible();
    await expect(sidePanel(page).getByText("Here beside you.")).toBeVisible({
      timeout: RUN_STEPS_MS,
    });
  });
});

// SLOW(>=2s): a run in the panel, its widening to the content area and its docking, a draft.
test("Widen fills the content area beside the navigation and Dock puts the panel back, with the same run and the unsent words", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  await seedList();
  const asked = "what is on this list";
  await scriptChat({
    default: prose("A short answer while the person pauses."),
    runs: [{ turns: [say("Two issues about the sky.")], words: asked }],
  });
  await withAlice(browser, async (page) => {
    // From the width at which the navigation stands beside the page: below it the panel keeps its
    // side width and offers no widening.
    test.skip((page.viewportSize()?.width ?? 0) < 1280, "the navigation is a drawer below 1280 px");
    const started = countStartedRuns(page);
    await openBarOver(page, "/projects/CORE/issues");
    await askTheAgent(page, asked);
    await expectHandedToThePanel(page);
    await expect(newestRun(page)).toHaveAttribute("data-state", "done", { timeout: RUN_STEPS_MS });
    const panel = sidePanel(page);
    const run = await newestRun(page).getAttribute("data-run");
    await conversationBox(page).fill("half a thought");

    const docked = await panel.boundingBox();
    expect(docked?.width).toBeCloseTo(420, 0);
    await panel.getByRole("button", { name: "Widen the panel" }).click();
    await expect(panel).toHaveAttribute("data-wide", "true");
    const widened = await panel.boundingBox();
    const navigation = await page.getByRole("complementary", { name: "Navigation" }).boundingBox();
    // From the edge of the navigation to the window's right edge, so the navigation stays beside
    // it and reachable.
    expect(widened?.x).toBeCloseTo((navigation?.x ?? 0) + (navigation?.width ?? 0), 0);
    expect((widened?.x ?? 0) + (widened?.width ?? 0)).toBeCloseTo(
      page.viewportSize()?.width ?? 0,
      0
    );
    // The same run, its answer and the unsent words, the focus on the button just pressed, and no
    // second run.
    await expect(newestRun(page)).toHaveAttribute("data-run", run ?? "");
    await expect(sidePanel(page).getByTestId("going-run")).toHaveCount(1);
    await expect(conversationBox(page)).toHaveValue("half a thought");
    await expect(panel.getByRole("button", { name: "Dock at the side" })).toBeFocused();
    expect(started.bodies).toHaveLength(1);
    // The navigation is reachable beside the widened panel.
    await page
      .getByRole("link", { name: /^Inbox/ })
      .first()
      .click();
    await expect(page).toHaveURL(/\/$/);
    await expect(panel).toBeVisible();

    await panel.getByRole("button", { name: "Dock at the side" }).click();
    await expect(panel).toHaveAttribute("data-wide", "false");
    expect((await panel.boundingBox())?.width).toBeCloseTo(420, 0);
    await expect(newestRun(page)).toHaveAttribute("data-run", run ?? "");
    await expect(sidePanel(page).getByTestId("going-run")).toHaveCount(1);
    await expect(conversationBox(page)).toHaveValue("half a thought");
    expect(started.bodies).toHaveLength(1);
  });
});

// SLOW(>=2s): Jev's Do for the words, and Enter on it.
test("Enter on the line Do keeps its conversation in the bar, and opens no panel", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  await seedList();
  await scriptDecision({ default: decided("dispatch_issue_update", 0.99) });
  const words = "mark the comet work as started";
  await scriptChat({
    default: prose("On it."),
    runs: [{ turns: [say("Nothing to change.")], words }],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await searchBox(page).fill(words);
    await expect(lineOnTop(page)).toContainText("Do", { timeout: RUN_STEPS_MS });
    await searchBox(page).press("Enter");
    await expect(newestRun(page)).toBeVisible({ timeout: RUN_STEPS_MS });
    await expect(bar(page)).toHaveAttribute("data-state", "conversation");
    await expect(sidePanel(page)).toHaveCount(0);
    await expect(newestRun(page).getByTestId("going-run-answer")).toHaveText("Nothing to change.", {
      timeout: RUN_STEPS_MS,
    });
  });
});

// Firefox keeps Ctrl-Shift-K for its Web Console. The row tells whether the page gets that key in
// a real Firefox, and records in its report which key kept the conversation at the side: where
// the page gets it, Ctrl-Shift-K; where it does not, Ctrl-K pressed while the bar is open; and the
// button "Keep at the side" stays either way.
// SLOW(>=2s): one open of the bar and a move to the side.
test("in Firefox the conversation keeps at the side by Ctrl-Shift-K, or else by Ctrl-K in the open bar, and the button stays @firefox", async ({
  browser,
}) => {
  modelsLane();
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/");
    await expect(bar(page).getByRole("button", { name: "Keep at the side" })).toBeVisible();
    await searchBox(page).press("Control+Shift+K");
    let kept = "Ctrl-Shift-K";
    // The press returns once the page handled the key, and React draws a key's move in that task.
    if ((await sidePanel(page).count()) === 0) {
      kept = "Ctrl-K in the open bar";
      await searchBox(page).press("Control+k");
    }
    await expect(sidePanel(page)).toBeVisible();
    await expect(bar(page)).toHaveCount(0);
    test.info().annotations.push({
      description: kept,
      type: "the key that kept the conversation at the side",
    });
  });
});
