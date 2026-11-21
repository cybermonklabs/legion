import { setInterests, setLiveSessions } from "./agents";
import { createComment, getAsk, getIssue, listAskFollowers, requestApproval } from "./api";
import {
  askNow,
  asTester,
  badgeOf,
  defineRule,
  type Hand,
  keptRewrite,
  openSurface,
  rewriteRule,
  type Seeded,
  SURFACES,
  type Surface,
  seedProject,
  seedSurface,
  selectWords,
  TESTER,
} from "./ask-card";
import { setModelRules } from "./fake-model-helpers";
import { inboxRow, READING_PANE_WIDTH } from "./inbox-pane";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * The ask card's one control and its keys (INFRA-3231's R7, check 1c7c0589): the rows of
 * INFRA-3552's check c6 on each surface, with the check's changes. The card has one split button
 * - Answer, a thin divider and a chevron - and no other button that opens its menu; the chevron
 * opens it even while Answer cannot be pressed; the menu holds Rewrite, Ask back, Hand this
 * question to…, Decline to answer and Define selected words, in that order, in three groups, each
 * with its key; an approval request's holds Ask back, Hand this question to… and Define selected
 * words. The card shows no urgency label but on a decision block, no Asked of label and no
 * separate Decline button, names whose turn it is only when it is not the reader's, and names who
 * last handed the question on and when. The chevron beside the asking agent's name opens the
 * Reaches list, whose Unfollow and Unsubscribe take a session off it. With the card, row or block
 * in hand, `1`-`9`, Enter, `r`, `b`, `n` and `a` act; inside the answer box they type.
 *
 * c6's row that reads the keymap registry is the web app's unit test
 * (`web/src/features/shell/keymap.test.ts`): it reads no page.
 *
 * At the commit before the change this file fails: the card has no split button, and `r`, `b`,
 * `n` and `a` do nothing.
 */

/** The menu's items in order, and the key each one shows. */
const MENU = [
  ["rewrite", "Rewrite", "r"],
  ["ask-back", "Ask back", "b"],
  ["hand-on", "Hand this question to…", "a"],
  ["decline", "Decline to answer", "n"],
  ["define", "Define selected words", "d"],
] as const;

function menu(page: Page): Locator {
  return page.locator("[data-ask-menu]");
}

/** The card's one split button: its action and its chevron, and no other control that opens a
 *  menu. */
async function expectOneSplitButton(card: Locator): Promise<void> {
  await expect(card.locator("[data-ask-action]")).toHaveCount(1);
  await expect(card.locator("[data-ask-menu-button]")).toHaveCount(1);
  await expect(card.locator('[aria-haspopup="menu"]')).toHaveCount(1);
  await expect(card.getByRole("button", { name: /Decline/ })).toHaveCount(0);
  await expect(card).not.toContainText("Asked of");
}

/** Opens the menu by the chevron and reads its groups, its items and their keys. */
async function expectMenu(
  page: Page,
  card: Locator,
  items: readonly (readonly [string, string, string])[]
): Promise<void> {
  await card.locator("[data-ask-menu-button]").click();
  await expect(menu(page)).toBeVisible();
  await expect(menu(page).locator("[data-ask-menu-item]")).toHaveCount(items.length);
  const ids = await menu(page)
    .locator("[data-ask-menu-item]")
    .evaluateAll((all) => all.map((item) => item.getAttribute("data-ask-menu-item")));
  expect(ids).toEqual(items.map(([id]) => id));
  for (const [id, label, key] of items) {
    const item = menu(page).locator(`[data-ask-menu-item="${id}"]`);
    await expect(item).toContainText(label);
    await expect(item.locator("kbd")).toHaveText(key);
  }
  // Define is greyed until words of the question are selected.
  await expect(menu(page).locator('[data-ask-menu-item="define"]')).toHaveAttribute(
    "aria-disabled",
    "true"
  );
  await page.keyboard.press("Escape");
  await expect(menu(page)).toHaveCount(0);
}

/** c6 on one surface: the one split button and its menu, what the card leaves out, and the keys
 *  1, Enter (on the Inbox, where the row is the thing in hand), r, b, n and a. */
async function menuOn(page: Page, surface: Surface): Promise<void> {
  await seedProject();
  const seeded = await seedSurface(surface);
  await setModelRules([rewriteRule(await keptRewrite(seeded.askId), { delayMs: 1_000 })]);
  const hand = await openSurface(page, surface, seeded);
  const { card } = hand;
  await expectOneSplitButton(card);
  // Answer cannot be pressed before a pick, and the chevron opens the menu all the same.
  await expect(card.locator("[data-ask-action]")).toBeDisabled();
  await hand.grab();
  await expectMenu(page, card, MENU);

  // The urgency is the card's edge alone, but a decision block keeps the header the design draws.
  if (surface === "decision block") {
    await expect(hand.frame).toContainText("High");
  } else {
    await expect(card.getByText("High", { exact: true })).toHaveCount(0);
    await expect(card).toHaveAttribute("aria-label", "Urgency: High");
  }
  // Whose turn it is, only where it is not the reader's: the reply thread's question waits on bob.
  if (surface === "reply thread") {
    await expect(card.getByTestId(`turn-${seeded.askId}`)).toContainText("Waiting on bob");
  } else {
    await expect(card.getByTestId(`turn-${seeded.askId}`)).toHaveCount(0);
  }

  // 1 picks the first option.
  await hand.grab();
  await page.keyboard.press("1");
  await expect(card.locator("[data-ask-option]").first()).toBeChecked();
  await expect(card.locator("[data-ask-action]")).toBeEnabled();

  // Enter, from the row the Inbox holds, puts the cursor in the answer box.
  if (surface === "inbox row" || surface === "reading pane") {
    await hand.grab();
    await page.keyboard.press("Enter");
    await expect(card.locator("[data-ask-answer]")).toBeFocused();
  }

  // b asks back: the cursor in the answer box, the action reading Ask back; Escape answers again.
  await hand.grab();
  await page.keyboard.press("b");
  const box = card.locator("[data-ask-answer]");
  await expect(box).toBeFocused();
  await expect(box).toHaveAttribute("data-asking-back", "true");
  await expect(card.locator("[data-ask-action]")).toHaveText("Ask back");
  await page.keyboard.press("Escape");
  await expect(box).toHaveAttribute("data-asking-back", "false");
  await expect(card.locator("[data-ask-action]")).toHaveText("Answer");

  // n opens Decline to answer; a opens Hand this question to.
  await hand.grab();
  await page.keyboard.press("n");
  const decline = page.getByRole("dialog", { name: "Decline to answer" });
  await expect(decline).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(decline).toHaveCount(0);
  await hand.grab();
  await page.keyboard.press("a");
  const picker = page.getByRole("dialog", { name: "Hand this question to" });
  await expect(picker).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(picker).toHaveCount(0);

  // r rewrites.
  await hand.grab();
  await page.keyboard.press("r");
  await expect(badgeOf(hand.frame)).toHaveText(/Rewriting…|Rewritten/);
  await expect(badgeOf(hand.frame)).toHaveText("Rewritten", { timeout: 20_000 });
}

for (const surface of SURFACES) {
  // SLOW(>=2s): a seed, a page, the menu and five keys, each opening something and closing it.
  test(`on the ${surface} the card's one split button holds every action, and its keys act`, async ({
    browser,
  }) => {
    const alice = await asUser(browser, "alice");
    try {
      await menuOn(await alice.newPage(), surface);
    } finally {
      await alice.close();
    }
  });
}

/** The reading pane on one question of an issue alice holds. */
async function paneSeed(browser: Parameters<typeof asUser>[0]) {
  await seedProject();
  const seeded = await seedSurface("reading pane");
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  const hand = await openSurface(page, "reading pane", seeded);
  return { alice, hand, page, seeded };
}

// SLOW(>=2s): two questions asked back, each a write the thread reads back.
test("b keeps the text, Ask back sends it as a comment with either modified Enter, and Escape answers again", async ({
  browser,
}) => {
  const { alice, hand, page, seeded } = await paneSeed(browser);
  try {
    const box = hand.card.locator("[data-ask-answer]");
    await box.fill("Which mirror do you mean?");
    await hand.grab();
    await page.keyboard.press("b");
    await expect(box).toBeFocused();
    await expect(box).toHaveValue("Which mirror do you mean?");
    await expect(hand.card.locator("[data-ask-action]")).toHaveText("Ask back");
    await page.keyboard.press("Control+Enter");
    await expect(box).toHaveValue("");
    await expect
      .poll(async () => ((await getAsk(seeded.askId)).replies ?? []).map((reply) => reply.body))
      .toEqual(["Which mirror do you mean?"]);
    // The question stays open, and the turn is the agent's.
    expect((await askNow(seeded.askId)).state).toBe("open");
    await expect(hand.card.getByTestId(`turn-${seeded.askId}`)).toContainText("Waiting on");

    await hand.grab();
    await page.keyboard.press("b");
    await box.fill("And which ledger?");
    await page.keyboard.press("Meta+Enter");
    await expect.poll(async () => ((await getAsk(seeded.askId)).replies ?? []).length).toBe(2);
    expect((await askNow(seeded.askId)).state).toBe("open");

    await hand.grab();
    await page.keyboard.press("b");
    await expect(box).toHaveAttribute("data-asking-back", "true");
    await page.keyboard.press("Escape");
    await expect(box).toHaveAttribute("data-asking-back", "false");
    await expect(hand.card.locator("[data-ask-action]")).toHaveText("Answer");
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): a definition, then three Escapes read one at a time.
test("with a definition open and the box asking back, Escape closes the definition, then answers again, then goes back to the row", async ({
  browser,
}) => {
  const { alice, hand, page, seeded } = await paneSeed(browser);
  try {
    await setModelRules([defineRule("flip audit", "The check of the ledger's mirror.", [])]);
    await hand.grab();
    await selectWords(hand.question, "flip audit");
    await page.keyboard.press("d");
    const definition = page.locator("[data-define-popover]");
    await expect(definition).toContainText("The check of the ledger's mirror.");
    await page.keyboard.press("b");
    const box = hand.card.locator("[data-ask-answer]");
    await expect(box).toHaveAttribute("data-asking-back", "true");

    await page.keyboard.press("Escape");
    await expect(definition).toHaveCount(0);
    await expect(box).toHaveAttribute("data-asking-back", "true");
    await page.keyboard.press("Escape");
    await expect(box).toHaveAttribute("data-asking-back", "false");
    await page.keyboard.press("Escape");
    await expect(inboxRow(page, seeded.askId)).toBeFocused();
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): a page, and every key of the card typed into its box.
test("inside the answer box the card's keys type text", async ({ browser }) => {
  const { alice, hand, page } = await paneSeed(browser);
  try {
    const box = hand.card.locator("[data-ask-answer]");
    await box.focus();
    await page.keyboard.type("rbnad1w");
    await expect(box).toHaveValue("rbnad1w");
    await expect(page.getByRole("dialog")).toHaveCount(0);
    await expect(menu(page)).toHaveCount(0);
    await expect(badgeOf(hand.card)).toHaveCount(0);
    await expect(hand.card.locator("[data-ask-option]").first()).not.toBeChecked();
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): an issue page, and two presses of one key, the second a priority write.
test("on an issue page 2 picks the card's second option with the card in hand, and sets the priority without it", async ({
  browser,
}) => {
  await seedProject();
  const seeded = await seedSurface("conversation");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Every change of the issue's fields the page sends, the priority's among them.
    const patches: string[] = [];
    page.on("request", (request) => {
      if (
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${seeded.issueKey}`
      ) {
        patches.push(request.postData() ?? "");
      }
    });
    const hand = await openSurface(page, "conversation", seeded);
    await hand.grab();
    await page.keyboard.press("2");
    await expect(hand.card.locator("[data-ask-option]").nth(1)).toBeChecked();
    // The keys' handlers are synchronous, so a priority write the press let through had left
    // before a request the page issues next is answered: that answer is the barrier.
    await page.evaluate(() => fetch("/auth/whoami").then(({ status }) => status));
    expect(patches).toHaveLength(0);
    await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
    await page.keyboard.press("2");
    await expect.poll(() => getIssue(seeded.issueKey)).toMatchObject({ priority: 2 });
    expect(patches).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

/** The card's keys as the shortcut help and the command bar label them. */
const CARD_KEYS = [
  "Pick an option",
  "Put the cursor in the answer box",
  "Rewrite",
  "Ask back",
  "Hand this question to…",
  "Decline to answer",
  "Define selected words",
  "Show or hide the original wording",
  "Send the answer, or the question back",
  "Close a definition, or stop asking back",
];

// SLOW(>=2s): a rewrite, so `w` acts, then the help and the command bar each opened once.
test("the shortcut help lists the card's keys under one heading, and the command bar lists them for the card in hand", async ({
  browser,
}) => {
  const { alice, hand, page, seeded } = await paneSeed(browser);
  try {
    await setModelRules([rewriteRule(await keptRewrite(seeded.askId))]);
    await hand.grab();
    await page.keyboard.press("r");
    await expect(badgeOf(hand.frame)).toHaveText("Rewritten", { timeout: 20_000 });

    await hand.grab();
    await page.keyboard.press("Shift+Slash");
    const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(help).toBeVisible();
    await expect(help.getByRole("heading", { exact: true, name: "Ask card" })).toHaveCount(1);
    const section = help.getByRole("region", { name: "Ask card" });
    for (const label of CARD_KEYS) {
      await expect(section.getByText(label, { exact: true }).first()).toBeVisible();
    }
    await page.keyboard.press("Escape");
    await expect(help).toHaveCount(0);

    await hand.grab();
    await selectWords(hand.question, "flip audit");
    await page.keyboard.press("Control+k");
    const actions = page
      .getByRole("dialog", { name: "Search" })
      .getByRole("group", { name: "Actions" });
    await expect(actions).toBeVisible();
    for (const label of [
      "Pick option 1",
      "Pick option 2",
      "Put the cursor in the answer box",
      "Rewrite",
      "Ask back",
      "Decline to answer",
      "Hand this question to…",
      "Define selected words",
      "Show or hide the original wording",
    ]) {
      await expect(actions.getByRole("option", { name: label }).first(), label).toBeVisible();
    }
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): an approval request, its menu, two keys that must do nothing and a definition.
test("an approval request's chevron opens Ask back, Hand this question to and Define; r and n do nothing, and d defines", async ({
  browser,
}) => {
  await seedProject();
  const seeded: Seeded = await seedSurface("conversation");
  const approval = await requestApproval(
    seeded.artifactId,
    { summary: "The flip audit plan." },
    asTester
  );
  const id = approval.ask?.id ?? "";
  expect(id).not.toBe("");
  await setModelRules([defineRule("Approve", "Say whether the plan may go ahead.", [])]);
  // Where the Inbox has its reading column, the column draws an approval request's document and
  // the head above it rather than a card; below that width every row draws its own card, which
  // is where the request's split button is.
  const alice = await asUser(browser, "alice", {
    viewport: { height: 900, width: READING_PANE_WIDTH - 1 },
  });
  try {
    const page = await alice.newPage();
    const rewrites: string[] = [];
    page.on("request", (request) => {
      if (request.url().endsWith(`/api/v1/asks/${id}/rewrites`)) rewrites.push(request.url());
    });
    await page.goto("/");
    const card = inboxRow(page, id).locator(`[data-ask-card="${id}"]`);
    await expect(card).toBeVisible();
    await expectOneSplitButton(card);
    await inboxRow(page, id).focus();
    await expectMenu(page, card, [
      ["ask-back", "Ask back", "b"],
      ["hand-on", "Hand this question to…", "a"],
      ["define", "Define selected words", "d"],
    ]);

    await inboxRow(page, id).focus();
    await page.keyboard.press("r");
    await page.keyboard.press("n");
    // The keys' handlers are synchronous, so a write either let through had left before a
    // request the page issues next is answered: that answer is the barrier.
    await page.evaluate(() => fetch("/auth/whoami").then(({ status }) => status));
    expect(rewrites).toHaveLength(0);
    await expect(page.getByRole("dialog", { name: "Decline to answer" })).toHaveCount(0);
    await expect(badgeOf(card)).toHaveCount(0);

    await selectWords(card.locator("[data-ask-question]"), "Approve");
    await page.keyboard.press("d");
    await expect(page.locator("[data-define-popover]")).toContainText(
      "Say whether the plan may go ahead."
    );
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): a hand-on written and read back on the card.
test("a question handed on names who handed it on and when, and whose turn it is", async ({
  browser,
}) => {
  await seedProject();
  const seeded = await seedSurface("conversation");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const hand = await openSurface(page, "conversation", seeded);
    await hand.grab();
    await page.keyboard.press("a");
    const picker = page.getByRole("dialog", { name: "Hand this question to" });
    await picker.locator('[data-ask-hand-on-person="bob"]').click();
    await expect(hand.card.locator("[data-ask-handed-on]")).toContainText("handed on by alice");
    await expect(hand.card.getByTestId(`turn-${seeded.askId}`)).toContainText("Waiting on bob");
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): two removals confirmed and read back, from the popover the asker's chevron opens.
test("the chevron beside the asking agent's name opens the Reaches list, whose Unfollow and Unsubscribe take a session off", async ({
  browser,
}) => {
  await seedProject();
  const seeded = await seedSurface("reading pane");
  const replier = { id: "legion-demo-demo-1-reviewer", kind: "session" as const };
  const watcher = "legion-demo-watcher";
  await createComment(
    seeded.issueKey,
    { ask_id: seeded.askId, body: "The mirror run is green on my side." },
    { actor: replier, as: "agent" }
  );
  await setLiveSessions([
    TESTER,
    { session_id: replier.id, title: "DEMO-1 reviewer" },
    { session_id: watcher, title: "Watcher" },
  ]);
  await setInterests([
    { session_id: watcher, topics: [`notifications.dispatch.issue.${seeded.issueKey}.>`] },
  ]);
  const alice = await asUser(browser, "alice", { viewport: { height: 900, width: 1440 } });
  try {
    const page = await alice.newPage();
    const hand: Hand = await openSurface(page, "reading pane", seeded);
    // No line of the card carries the list: it is behind the chevron.
    await expect(hand.card.getByRole("region", { name: "Recipients" })).toHaveCount(0);
    await hand.card.locator("[data-ask-asker]").click();
    const recipients = page.getByRole("dialog").getByRole("region", { name: "Recipients" });
    await expect(recipients).toContainText("Reaches 3");
    const followers = recipients.getByRole("list", { name: "Followers" });
    await expect(followers.locator(`[title='${TESTER.session_id}']`)).toHaveCount(1);
    await followers
      .getByRole("listitem")
      .filter({ has: page.locator(`[title='${replier.id}']`) })
      .getByRole("button", { name: "Unfollow" })
      .click();
    await page
      .getByRole("dialog", { name: "Unfollow" })
      .getByRole("button", { name: "Confirm" })
      .click();
    await expect
      .poll(async () =>
        ((await listAskFollowers(seeded.askId)).followers ?? []).map((row) => row.session_id)
      )
      .toEqual([TESTER.session_id]);
    await expect(recipients.locator(`[title='${replier.id}']`)).toHaveCount(0);

    const viaIssue = recipients.getByRole("list", { name: "Via issue subscription" });
    await viaIssue.getByRole("button", { name: "Unsubscribe" }).click();
    await page
      .getByRole("dialog", { name: "Unsubscribe" })
      .getByRole("button", { name: "Confirm" })
      .click();
    await expect(recipients.locator(`[title='${watcher}']`)).toHaveCount(0);
    await expect(recipients).toContainText("Reaches 1");
  } finally {
    await alice.close();
  }
});
