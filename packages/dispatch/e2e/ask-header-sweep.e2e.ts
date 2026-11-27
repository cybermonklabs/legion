import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  declineAsk,
  getAsk,
  patchIssue,
  resolveAsk,
} from "./api";
import { type Drawn, holds, holdsAt } from "./ask-header-reader";
import {
  ANSWERED_BLOCK,
  asker,
  cardHeader,
  containerWidth,
  DEFAULT_FONT,
  resetToPlanner,
  seedAnsweredBlock,
} from "./ask-header-rows";
import { expectRootFont, LARGE_FONT, setDefaultFont, XL } from "./default-font";
import { documentEditor, indexedBlockAsk, marginCard, setSheet } from "./editor";
import { askCard, inboxPane, inboxRow, readingView } from "./inbox-pane";
import { asUser } from "./users";

/**
 * INFRA-341, R1: one ask header, with the same fields, wherever an ask is drawn, and each place
 * keeping its own layout. Each place below draws every state it draws, and each header is swept
 * through its own width, from 216 px to the widest it is drawn at, 4 px at a time, the way a host
 * of that width would draw it: the header reads its own width (`@container`) and nothing else,
 * so a host can only change the header's width. At every step the row asserts five things of
 * what the page draws:
 *
 * - no drawn part of the header overlaps another;
 * - no drawn part crosses the header's own edge, read from the text's own ranges, clipped where
 *   a box clips it, and never from the box around the text: a time spilling out of a `min-w-0`
 *   box that has shrunk to nothing is drawn outside that box, and the box alone says nothing;
 * - the copy control is on the first line, the line of the row's first part;
 * - every field the header draws there is drawn and visible, a name that truncates keeping at
 *   least 12 px of itself;
 * - the row draws the lines its place's layout draws at that width (`Lines`), so a header
 *   stacked into rows where its place draws one line fails, as a header that never wraps does.
 *
 * Each project sweeps at its own size of control: the desktop's 32 px boxes, and the phone's
 * 44 px touch boxes, which every button is below `xl` (`web/src/styles.css`), 1280 px at the
 * browser's own 16 px default font, a tablet's width included. That is why the phone sweeps past
 * its own width, to 960 px: a header at a tablet's width has the phone's boxes.
 *
 * The sweeps read at that 16 px default. Two rows read again at a reader's own 20 px one, set
 * through Chromium's `Page.setFontSizes` and so run under `chromium` alone, where every `rem` is
 * 20 px and every width a layout switches at moves with it: the three container sizes
 * (`e2e/ask-header-rows.ts`), and `xl`. They read each header just under and at each container
 * size alone rather than every width, on both sides of `xl`, and hold the same five assertions
 * there.
 *
 * The rows that read headers at the one width a reader meets each at are
 * `e2e/ask-header-at-width.e2e.ts`'s, and hold the same five assertions there. What the rows of
 * both files share - the session that asks, the answered block they seed, the widths a layout
 * switches at and what a card's and that block's headers draw - is `e2e/ask-header-rows.ts`'s.
 *
 * Each row's failures name the widths they were read at. Three controls were run against these
 * rows and those of `e2e/ask-header-at-width.e2e.ts`, each from a build of its own: round 8's
 * copy control wrapping as one more item of its line, round 9's stacked layout at every width,
 * and round 10's one line that never wraps at every width. Each control's diff, and every row it
 * reddened with the widths and the failure messages, are recorded in
 * dispatch://INFRA-341/artifact/r16-header-controls-txt.
 */

test.beforeEach(resetToPlanner);

/** The header widths a layout switches at, at a root whose `rem` is `rem` px: just under each of
 *  the three container sizes, and at it. */
function switchWidths(rem: number): number[] {
  return ["ask-card-two-lines", "ask-card-line", "ask-block-line"].flatMap((name) => {
    const width = containerWidth(name, rem);
    return [width - 1, width];
  });
}

/** What `drawn` draws at a root whose `rem` is `rem` px rather than the browser's own: each band
 *  of its lines starts where its container size does, and a container size is a size in `rem`. */
function atRem(drawn: Drawn, rem: number): Drawn {
  return {
    ...drawn,
    lines: drawn.lines?.map(
      ([from, least, most]) => [(from / DEFAULT_FONT) * rem, least, most] as const
    ),
  };
}

/** The answered and resolved records' own parts (`RecordAskHeader`). */
function recordHeader(options: { anchored: boolean; notched: boolean }): Drawn {
  return {
    fields: {
      asker: "[data-ask-record-row] [data-agent-name]",
      copy: "[data-copy-ref] button",
      label: "[data-ask-state-label]",
      time: "[data-ask-record-row] time",
      ...(options.notched ? { urgency: "[data-ask-urgency-notch]" } : {}),
      ...(options.anchored ? { anchor: ":scope > blockquote" } : {}),
    },
    row: "[data-ask-record-row]",
  };
}

/** An open issue holding every state a card and a record draw, each ask anchored in the spec and
 *  asked by the long-titled session: one waiting on the reader, blocking, so its card draws the
 *  urgency notch; one whose turn the reader's question handed back to the session; one the
 *  reader answered, high, so its record draws the notch too; one the reader resolved; one the
 *  session retracted, whose label is the longest word a resolution draws; and one the reader
 *  declined to answer, through the route the card's **Decline to answer** writes to. */
async function seedAsks() {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nThe mirror replays the ledger on a cold start, and the budget holds until the window closes. The owner signs the release.\n",
    title: "Header sweep",
  });
  const anchored = (quote: string) => ({ artifact: "spec", quote });
  const yours = await createAsk(
    issue.key,
    {
      anchor: anchored("replays the ledger"),
      question: "Does a cold start replay the ledger?",
      urgency: "blocking",
    },
    asker
  );
  const theirs = await createAsk(
    issue.key,
    { anchor: anchored("cold start"), question: "Which mirror keeps the budget?", urgency: "high" },
    asker
  );
  await createComment(issue.key, { ask_id: theirs.id, body: "Which mirror do you mean?" });
  await expect
    .poll(() => getAsk(theirs.id), { intervals: [50, 50, 100, 200, 500] })
    .toMatchObject({ ask: { waiting_on: "agent" } });
  const answered = await createAsk(
    issue.key,
    { anchor: anchored("the budget holds"), question: "Does the budget hold?", urgency: "high" },
    asker
  );
  await answerAsk(answered.id, { expected_edited_at: null, selected: [], text: "It holds." });
  const resolved = await createAsk(
    issue.key,
    { anchor: anchored("the window closes"), question: "Should the window close?" },
    asker
  );
  await resolveAsk(resolved.id, { kind: "resolved", reason: "Decided elsewhere." });
  const retracted = await createAsk(
    issue.key,
    { anchor: anchored("The mirror"), question: "Is the mirror the ledger?" },
    asker
  );
  await resolveAsk(retracted.id, { kind: "retracted", reason: "Superseded." }, asker);
  const declined = await createAsk(
    issue.key,
    { anchor: anchored("The owner signs"), question: "Who signs the release?" },
    asker
  );
  await declineAsk(declined.id, { note: "Ask the release owner." });
  return { answered, declined, issue, resolved, retracted, theirs, yours };
}

/** Opens a page as alice with its clock held 59 minutes after the seed, so every time a header
 *  draws reads "59 minutes ago", the widest time it draws in its first day. */
async function alicePage(browser: Browser) {
  const alice = await asUser(browser, "alice");
  const page = await alice.newPage();
  await page.clock.setFixedTime(Date.now() + 59 * 60_000);
  return { alice, page };
}

/** The card a record turns into while its answerer changes the answer: the card's header with
 *  the answered state's label, and no turn. */
async function changing(host: Locator): Promise<Locator> {
  await host.getByRole("button", { name: "Change answer" }).click();
  await expect(host.getByRole("button", { name: "Save answer" })).toBeVisible();
  return host.locator("[data-ask-header]").first();
}

// SLOW(>=2s): six asks seeded, then the Inbox and the Answers page each loaded once and three
// headers swept through 187 widths each.
test("the Inbox card's header holds its layout from 216 px to its widest, whoever's turn it is and while an answer changes @phone", async ({
  browser,
}) => {
  const { answered, theirs, yours } = await seedAsks();
  const { alice, page } = await alicePage(browser);
  try {
    await page.goto("/");
    for (const [state, ask, turn] of [
      ["waiting on you", yours, true],
      ["an agent's turn", theirs, true],
    ] as const) {
      const card = await askCard(page, ask.id);
      await holds(
        card.locator("[data-ask-header]").first(),
        cardHeader({ anchored: true, notched: true, turn }),
        `the Inbox card, ${state}`
      );
    }
    await page.goto("/answers");
    await page
      .locator(`[data-answer-row="${answered.id}"]`)
      .first()
      .getByRole("button", { name: "Change answer" })
      .click();
    const card = page.locator(`[data-answer-card="${answered.id}"]`);
    await expect(card.getByRole("button", { name: "Save answer" })).toBeVisible();
    await holds(
      card.locator("[data-ask-header]").first(),
      cardHeader({ anchored: true, notched: true, turn: false }),
      "the Inbox card, answered, while the answer changes"
    );
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): six asks seeded, the Conversation loaded once, and three headers swept through
// 187 widths each.
test("the compact Inbox card's header holds its layout from 216 px to its widest, whoever's turn it is and while an answer changes @phone", async ({
  browser,
}) => {
  const { answered, issue, theirs, yours } = await seedAsks();
  const { alice, page } = await alicePage(browser);
  try {
    await page.goto(`/issues/${issue.key}/conversation`);
    for (const [state, ask] of [
      ["waiting on you", yours],
      ["an agent's turn", theirs],
    ] as const) {
      await holds(
        page.locator(`[data-turn="ask:${ask.id}"] [data-ask-header]`).first(),
        cardHeader({ anchored: true, notched: true, turn: true }),
        `the compact card, ${state}`
      );
    }
    await holds(
      await changing(page.locator(`[data-turn="ask:${answered.id}"]`)),
      cardHeader({ anchored: true, notched: true, turn: false }),
      "the compact card, answered, while the answer changes"
    );
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): six asks seeded, the issue page loaded once, and three headers swept through 187
// widths each.
test("the margin card's header holds its layout from 216 px to its widest, whoever's turn it is and while an answer changes @phone", async ({
  browser,
}, testInfo) => {
  const { answered, issue, theirs, yours } = await seedAsks();
  const { alice, page } = await alicePage(browser);
  try {
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("tab", { name: "Spec" }).click();
    await setSheet(page, testInfo.project.name, true);
    for (const [state, ask] of [
      ["waiting on you", yours],
      ["an agent's turn", theirs],
    ] as const) {
      await holds(
        marginCard(page, ask.id).locator("[data-ask-header]").first(),
        cardHeader({ anchored: true, notched: true, turn: true }),
        `the margin card, ${state}`
      );
    }
    await holds(
      await changing(marginCard(page, answered.id)),
      cardHeader({ anchored: true, notched: true, turn: false }),
      "the margin card, answered, while the answer changes"
    );
  } finally {
    await alice.close();
  }
});

/** A decision block's own header where a card under it draws the provenance: the urgency and
 *  `Decision`, on one line at every width. */
const CARDED_BLOCK: Drawn = {
  fields: { decision: "strong", urgency: "[data-dispatch-ask-pill]" },
  lines: [[0, 1, 1]],
  row: "header",
};

const BLOCKS =
  'Intro.\n\n:::ask{#yours urgency="blocking" multiple="false" state="open"}\nWhich way?\n\n- Left\n- Right\n:::\n\n:::ask{#theirs urgency="high" multiple="false" state="open"}\nWhich mirror?\n\n- Left\n- Right\n:::\n\n:::ask{#answered urgency="high" multiple="false" state="open"}\nShip it?\n\n- Ship\n- Hold\n:::\n\n:::ask{#declined urgency="high" multiple="false" state="open"}\nWho signs?\n\n- Owner\n- Planner\n:::\n';

// SLOW(>=2s): two issues of decision blocks seeded and indexed, each issue page loaded once, and
// eleven headers swept through 187 widths each.
test("a decision block's headers hold their layout from 216 px to their widest, open, answered, declined and resolved, and on a closed issue @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const open = await createIssue({ project: "CORE", spec: BLOCKS, title: "Open blocks" }, asker);
  const theirs = await indexedBlockAsk(open.key, "theirs");
  await createComment(open.key, { ask_id: theirs.id, body: "Which mirror do you mean?" });
  const answered = await indexedBlockAsk(open.key, "answered");
  await answerAsk(answered.id, { expected_edited_at: null, selected: ["Ship"], text: "Ship it." });
  const declined = await indexedBlockAsk(open.key, "declined");
  await declineAsk(declined.id, { note: "Ask the release owner." });
  // A closed issue's answered block draws its own header; its resolved block, whose close
  // resolved its ask, draws the resolution's record under a carded header.
  const closed = await createIssue(
    { project: "CORE", spec: BLOCKS, title: "Closed blocks" },
    asker
  );
  const closedAnswered = await indexedBlockAsk(closed.key, "answered");
  await answerAsk(closedAnswered.id, {
    expected_edited_at: null,
    selected: ["Ship"],
    text: "Ship it.",
  });
  await indexedBlockAsk(closed.key, "yours");
  await patchIssue(closed.key, { status: "done" });
  const { alice, page } = await alicePage(browser);
  try {
    await page.goto(`/issues/${open.key}`);
    for (const [id, state, hosted] of [
      ["yours", "waiting on you", "card"],
      ["theirs", "an agent's turn", "card"],
      ["answered", "answered", "record"],
      ["declined", "declined", "record"],
    ] as const) {
      const block = documentEditor(page).locator(`[data-dispatch-ask-block="${id}"]`);
      await expect(block.locator("article[data-testid^=ask-]")).toBeVisible();
      await holds(
        block.locator("header[data-ask-header]"),
        CARDED_BLOCK,
        `the block's header, ${state}`
      );
      await holds(
        block.locator("article [data-ask-header]").first(),
        hosted === "card"
          ? cardHeader({ anchored: false, notched: false, turn: true })
          : recordHeader({ anchored: false, notched: false }),
        `the block's ${hosted}, ${state}`
      );
    }

    await page.goto(`/issues/${closed.key}`);
    const answeredBlock = documentEditor(page).locator('[data-dispatch-ask-block="answered"]');
    await expect(answeredBlock.locator("[data-dispatch-ask-answered]")).toBeVisible();
    await holds(
      answeredBlock.locator("header[data-ask-header]"),
      ANSWERED_BLOCK,
      "a closed issue's answered block"
    );
    const resolvedBlock = documentEditor(page).locator('[data-dispatch-ask-block="yours"]');
    await expect(resolvedBlock.getByTestId("ask-resolution-badge")).toBeVisible();
    await holds(
      resolvedBlock.locator("header[data-ask-header]"),
      CARDED_BLOCK,
      "a closed issue's resolved block's header"
    );
    await holds(
      resolvedBlock.locator("article [data-ask-header]").first(),
      recordHeader({ anchored: false, notched: false }),
      "a closed issue's resolved block's record"
    );
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): six asks seeded, the Conversation loaded once, and four records swept through 187
// widths each.
test("the answered, resolved, retracted and declined records' headers hold their layout from 216 px to their widest @phone", async ({
  browser,
}) => {
  const { answered, declined, issue, resolved, retracted } = await seedAsks();
  const { alice, page } = await alicePage(browser);
  try {
    await page.goto(`/issues/${issue.key}/conversation`);
    for (const [state, ask, notched, label] of [
      ["answered", answered, true, "Answered"],
      ["resolved", resolved, false, "Resolved"],
      ["retracted", retracted, false, "Retracted"],
      ["declined", declined, false, "Declined"],
    ] as const) {
      const turn = page.locator(`[data-turn="ask:${ask.id}"]`);
      await expect(turn.locator("[data-ask-state-label]")).toHaveText(label);
      await holds(
        turn.locator("[data-ask-header]").first(),
        recordHeader({ anchored: true, notched }),
        `the ${state} record`
      );
    }
  } finally {
    await alice.close();
  }
});

/** The viewport widths on either side of `xl` at `LARGE_FONT`: one pixel under it, where every
 *  button is the 44 px touch box, and at it. Of the viewport's own switches `xl` is the one a
 *  header draws differently across: below it the touch floor (`web/src/styles.css`, unlayered,
 *  so it outranks the `md:` size a control's classes give it), and from it each control's own
 *  box. */
const XL_SIDES = [XL * LARGE_FONT - 1, XL * LARGE_FONT] as const;

/** Holds a switch-width row to what it says it reads, by what the page draws: the root at
 *  `LARGE_FONT`, and `header`'s copy control the 44 px touch box under `xl` and a box of its own
 *  from it. A row that read at the browser's own size, or on the other side of `xl`, fails here
 *  rather than passing for this one. */
async function readsAtLargeFont(header: Locator, compact: boolean): Promise<void> {
  await expectRootFont(header.page(), LARGE_FONT);
  const copy = header.locator("[data-copy-ref] button").first();
  if (compact) await expect(copy).toHaveCSS("min-height", "44px");
  else await expect(copy).not.toHaveCSS("min-height", "44px");
}

/** The question's card under the Inbox's reading column at `LARGE_FONT`: the Inbox lists short
 *  rows there and reads the row a reader opens in place of the list (INFRA-5200), so this opens
 *  the row's reading view, from the list, and answers the card the view draws. `askCard` reads
 *  which of the two the page draws off the viewport's width against the 16 px default's
 *  1280 px (`READING_PANE_WIDTH`), where the column stands from `XL * LARGE_FONT` px here. */
async function cardInReadingView(page: Page, askId: string): Promise<Locator> {
  const view = readingView(page);
  if ((await view.count()) > 0) {
    await view.getByRole("button", { exact: true, name: "Back to the Inbox list" }).click();
    await expect(view).toHaveCount(0);
  }
  await inboxRow(page, askId).locator("time").first().click();
  await expect(view.and(page.locator(`[data-inbox-reading-row="${askId}"]`))).toBeVisible();
  return inboxPane(page).getByTestId(`ask-${askId}`);
}

// SLOW(>=2s): six asks seeded, then on each side of `xl` the Inbox, the Conversation and the
// issue page each loaded once, and eleven headers read at six widths each.
test("at a 20 px default font the cards' and records' headers hold their layout just under and at each width it switches at, on both sides of xl", async ({
  browser,
}) => {
  const { answered, declined, issue, resolved, retracted, theirs, yours } = await seedAsks();
  const { alice, page } = await alicePage(browser);
  try {
    await setDefaultFont(page, LARGE_FONT);
    const widths = switchWidths(LARGE_FONT);
    const card = (turn: boolean) =>
      atRem(cardHeader({ anchored: true, notched: true, turn }), LARGE_FONT);
    for (const viewport of XL_SIDES) {
      const compact = viewport < XL * LARGE_FONT;
      const at = `at ${viewport} px with a ${LARGE_FONT} px default font`;
      await page.setViewportSize({ height: 900, width: viewport });

      await page.goto("/");
      for (const [state, ask] of [
        ["waiting on you", yours],
        ["an agent's turn", theirs],
      ] as const) {
        // The Inbox's reading column shows from `xl` (`READING_PANE_QUERY`), where `askCard`
        // selects the row and reads its card there. Under it the Inbox reads the row a reader
        // opens in place of the list, and the card is that view's (`cardInReadingView`).
        const host = compact ? await cardInReadingView(page, ask.id) : await askCard(page, ask.id);
        const header = host.locator("[data-ask-header]").first();
        await readsAtLargeFont(header, compact);
        await holdsAt(header, card(true), widths, `the Inbox card, ${state}, ${at}`);
      }

      await page.goto(`/issues/${issue.key}/conversation`);
      for (const [state, ask] of [
        ["waiting on you", yours],
        ["an agent's turn", theirs],
      ] as const) {
        const header = page.locator(`[data-turn="ask:${ask.id}"] [data-ask-header]`).first();
        await readsAtLargeFont(header, compact);
        await holdsAt(header, card(true), widths, `the compact card, ${state}, ${at}`);
      }
      for (const [state, ask, notched, label] of [
        ["answered", answered, true, "Answered"],
        ["resolved", resolved, false, "Resolved"],
        ["retracted", retracted, false, "Retracted"],
        ["declined", declined, false, "Declined"],
      ] as const) {
        const turn = page.locator(`[data-turn="ask:${ask.id}"]`);
        await expect(turn.locator("[data-ask-state-label]")).toHaveText(label);
        await holdsAt(
          turn.locator("[data-ask-header]").first(),
          atRem(recordHeader({ anchored: true, notched }), LARGE_FONT),
          widths,
          `the ${state} record, ${at}`
        );
      }
      await holdsAt(
        await changing(page.locator(`[data-turn="ask:${answered.id}"]`)),
        card(false),
        widths,
        `the compact card, answered, while the answer changes, ${at}`
      );

      await page.goto(`/issues/${issue.key}`);
      await page.getByRole("tab", { name: "Spec" }).click();
      // Under `xl` the margin is the review sheet over the document, drawn shut until the reader
      // opens it; from `xl` it is the column beside the document.
      if (compact) {
        const sheet = page.getByTestId("margin-sheet");
        await expect(sheet).toHaveAttribute("data-expanded", "false");
        await page.getByRole("button", { name: /^Open review panel/ }).click();
        await expect(sheet).toHaveAttribute("data-expanded", "true");
      }
      for (const [state, ask] of [
        ["waiting on you", yours],
        ["an agent's turn", theirs],
      ] as const) {
        const header = marginCard(page, ask.id).locator("[data-ask-header]").first();
        await readsAtLargeFont(header, compact);
        await holdsAt(header, card(true), widths, `the margin card, ${state}, ${at}`);
      }
    }
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): one decision block seeded, answered and its issue closed, then on each side of `xl`
// the issue page loaded once and its block's header read at six widths.
test("at a 20 px default font a closed issue's answered block's header holds its layout just under and at each width it switches at, on both sides of xl", async ({
  browser,
}) => {
  const { issue } = await seedAnsweredBlock();
  await patchIssue(issue.key, { status: "done" });
  const { alice, page } = await alicePage(browser);
  try {
    await setDefaultFont(page, LARGE_FONT);
    for (const viewport of XL_SIDES) {
      const compact = viewport < XL * LARGE_FONT;
      await page.setViewportSize({ height: 900, width: viewport });
      await page.goto(`/issues/${issue.key}`);
      const header = documentEditor(page).locator(
        '[data-dispatch-ask-block="decide"] header[data-ask-header]'
      );
      await expect(header.locator("[data-dispatch-ask-answered]")).toBeVisible();
      await readsAtLargeFont(header, compact);
      await holdsAt(
        header,
        atRem(ANSWERED_BLOCK, LARGE_FONT),
        switchWidths(LARGE_FONT),
        `a closed issue's answered block, at ${viewport} px with a ${LARGE_FONT} px default font`
      );
    }
  } finally {
    await alice.close();
  }
});
