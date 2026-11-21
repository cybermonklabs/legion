import { readFileSync } from "node:fs";

import { type Browser, devices, expect, type Locator, test } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
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
import {
  type Drawn,
  type FieldReading,
  holds,
  holdsAt,
  type Lines,
  type Reading,
  type Rect,
  readAtHostWidth,
} from "./ask-header-reader";
import { expectRootFont, LARGE_FONT, setDefaultFont, XL } from "./default-font";
import { documentEditor, indexedBlockAsk, marginCard, setSheet } from "./editor";
import { askCard, inboxPane } from "./inbox-pane";
import { replyRowId } from "./inbox-threads";
import { resetDatabase } from "./seed";
import { sessionIds } from "./session-ids";
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
 * 20 px and every width a layout switches at moves with it: the three container sizes below, and
 * `xl`. They read each header just under and at each container size alone rather than every
 * width, on both sides of `xl`, and hold the same five assertions there.
 *
 * The rows after the sweeps read headers at the one width a reader meets each at, through the
 * same reader (`readHeader`, `e2e/ask-header-reader.ts`, which holds what every row of this file
 * reads with and no row) a sweep reads each of its widths with: the Inbox card at 1280 px,
 * the margin card beside a 1280 px document, the Inbox card on an iPhone 13, a closed issue's
 * answered block at 1280, 1024, 768 and 390 px, and the same block in the Inbox's narrowest
 * reading pane. Each holds the five assertions there, and what that width draws: the layout the
 * row's width picks, the line each part stands on, and the block's height against `main`'s.
 *
 * Each row's failures name the widths they were read at. Three controls were run against this
 * file, each from a build of its own: round 8's copy control wrapping as one more item of its
 * line, round 9's stacked layout at every width, and round 10's one line that never wraps at
 * every width. Each control's diff, and every row it reddened with the widths and the failure
 * messages, are recorded in dispatch://INFRA-341/artifact/r16-header-controls-txt.
 */

/** The one session every ask here is asked by, and whose turn an agent's turn is: its title is
 *  the longest name a header draws, the name that has to give up its width. */
const planner: FakeSession = {
  capabilities: ["steer"],
  dir: "/workspaces/planner",
  last_seen: 1_700_000_000_000,
  machine_id: "e2e",
  roles: [],
  session_id: sessionIds.planner,
  title: "Release planner for the ledger mirror",
};

const asker = { actor: { id: sessionIds.planner, kind: "session" as const }, as: "agent" as const };

test.beforeEach(async () => {
  await resetDatabase();
  await setLiveSessions([planner]);
});

/** The sheet the three layout widths are named in, as container sizes of its `@theme`. */
const STYLESHEET = readFileSync(new URL("../web/src/styles.css", import.meta.url), "utf8");

/** The browser's own default font size, which every row reads at unless it says otherwise: the
 *  px of a `rem`, since nothing in `styles.css` sizes the root. */
const DEFAULT_FONT = 16;

/** The width `styles.css`'s `@theme` names `--container-<name>`, in px of a root whose `rem` is
 *  `rem` px: the one place each layout width is written, so these rows read the width the
 *  classes do. */
function containerWidth(name: string, rem = DEFAULT_FONT): number {
  const found = new RegExp(`--container-${name}:\\s*([\\d.]+)rem;`).exec(STYLESHEET);
  if (found?.[1] === undefined) {
    throw new Error(`web/src/styles.css's @theme names no --container-${name} in rem`);
  }
  return Number(found[1]) * rem;
}

/** From this width a read-only decision block's header is one line, below it rows
 *  (`--container-ask-block-line`, 36rem). */
const ASK_BLOCK_LINE = containerWidth("ask-block-line");
/** From this width an ask card's row is one line (`--container-ask-card-line`, 27.5rem). */
const ASK_CARD_LINE = containerWidth("ask-card-line");
/** From this width to `ASK_CARD_LINE` an ask card's row is two lines beside its copy control,
 *  and below it the phone's layout (`--container-ask-card-two-lines`, 19.5rem). */
const ASK_CARD_TWO_LINES = containerWidth("ask-card-two-lines");

/** The Inbox card's row (`CardAskHeader`): one line from `ASK_CARD_LINE`, two below it. */
const CARD_LINES: Lines = [
  [ASK_CARD_LINE, 1, 1],
  [0, 2, 2],
];

/** A read-only decision block's header (`BlockAskHeader`): from `ASK_BLOCK_LINE` one line, with
 *  "Answered by" on it or on the next; below, the urgency, `Decision` and the label - in two
 *  lines where even those do not fit beside the copy control - then who asked, then who
 *  answered. */
const ANSWERED_BLOCK_LINES: Lines = [
  [ASK_BLOCK_LINE, 1, 2],
  [0, 3, 4],
];

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

/** The header's own parts of a card, at each place a card is drawn. */
function cardHeader(options: { anchored: boolean; notched: boolean; turn: boolean }): Drawn {
  return {
    fields: {
      asked: "[data-ask-card-asked]",
      asker: "[data-ask-card-asked] span.truncate[title]",
      copy: "[data-copy-ref] button",
      label: "[data-ask-state-label]",
      "live link": '[data-ask-card-asked] a[aria-label^="Open live conversation"]',
      time: "[data-ask-card-asked] time",
      ...(options.turn ? { turn: '[data-testid^="turn-"]' } : {}),
      ...(options.notched ? { urgency: "[data-ask-urgency-notch]" } : {}),
      ...(options.anchored ? { anchor: ":scope > blockquote" } : {}),
    },
    lines: CARD_LINES,
    row: "[data-ask-card-row]",
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

/** A closed issue's answered block, whose own header draws everything. */
const ANSWERED_BLOCK: Drawn = {
  fields: {
    answered: "[data-dispatch-ask-answered]",
    asked: "[data-dispatch-ask-asked]",
    asker: "[data-dispatch-ask-asked] > [title]",
    copy: "[data-copy-ref] button",
    decision: "strong",
    label: "[data-ask-state-label]",
    time: "[data-dispatch-ask-asked] time",
    urgency: "[data-dispatch-ask-pill]",
  },
  lines: ANSWERED_BLOCK_LINES,
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
        // selects the row and reads its card there. Under it each row holds its own card, read
        // here directly: `askCard` takes the column to show from 1280 px, the 16 px default's.
        const host = compact ? page.getByTestId(`ask-${ask.id}`) : await askCard(page, ask.id);
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

/** A field a host-width row reads, which the header must draw: a reading without it fails the
 *  row, naming it. */
function drawnField(reading: Reading, name: string): FieldReading & { readonly span: Rect } {
  const field = reading.fields[name];
  if (field?.span == null) {
    throw new Error(`the header draws no ${name}: ${reading.failures.join("; ")}`);
  }
  return { ...field, span: field.span };
}

/** The first and the last line of its row a field stands on. */
function firstLine(field: FieldReading): number {
  return Math.min(...field.lines);
}

function lastLine(field: FieldReading): number {
  return Math.max(...field.lines);
}

/** Where a read-only decision block's header puts its parts at the width its host draws it, read
 *  as every sweep reads them: the lines they stand on, whether who asked shares the label's line
 *  or takes one under it, whether "Answered by" follows the copy control or takes a line under
 *  it, and the header's own height. And what is drawn rather than the boxes around it: a line too
 *  narrow for its parts drew the time past the end of who asked's own box, over the copy
 *  control, and pushed the control past the header's edge. */
async function blockLayout(header: Locator) {
  const reading = await readAtHostWidth(header, ANSWERED_BLOCK);
  const answered = drawnField(reading, "answered");
  const asked = drawnField(reading, "asked");
  const copy = drawnField(reading, "copy");
  const label = drawnField(reading, "label");
  const time = drawnField(reading, "time");
  const { head } = reading;
  return {
    answeredAfterCopy: answered.lines.includes(firstLine(label))
      ? answered.span.left >= copy.span.right
      : firstLine(answered) > Math.max(lastLine(copy), lastLine(label)),
    answeredUnderAsked: firstLine(answered) > lastLine(asked),
    askedOnLabelRow: asked.lines.includes(firstLine(label)),
    copyAtRightEnd: Math.abs(head.right - copy.span.right) <= 1,
    copyInsideHeader: copy.span.left >= head.left - 1 && copy.span.right <= head.right + 1,
    copyOnLabelRow: copy.lines.includes(firstLine(label)),
    failures: reading.failures,
    height: Math.round(head.bottom - head.top),
    rows: reading.lines,
    timeClearOfCopy:
      !time.lines.some((line) => copy.lines.includes(line)) ||
      time.span.right <= copy.span.left + 1,
    timeInsideHeader: time.span.right <= head.right + 1,
  };
}

/** An issue whose spec holds one decision block, asked by the long-titled session and answered,
 *  with the time the server says the answer was given. Each row closes the issue once it has
 *  seeded what it needs on it: a closed issue's answered block draws its own header, since no
 *  card under it carries the provenance. */
async function seedAnsweredBlock() {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue(
    {
      project: "CORE",
      spec: 'Intro.\n\n:::ask{#decide urgency="blocking" multiple="false" state="open"}\nWhich way?\n\n- Left\n- Right\n:::\n',
      title: "Closed block header",
    },
    asker
  );
  const blockAsk = await indexedBlockAsk(issue.key, "decide");
  const answered = await answerAsk(blockAsk.id, {
    expected_edited_at: null,
    selected: ["Left"],
    text: "Left.",
  });
  if (answered.answer === null) throw new Error("the answered ask holds no answer");
  return { answeredAt: answered.answer.at, blockAsk, issue };
}

/** What the block header's two layouts hold wherever they are drawn: none of the five failures,
 *  the time clear of the copy control and both inside the header, the control on the label's
 *  line. */
const BLOCK_HEADER_DRAWN = {
  copyInsideHeader: true,
  copyOnLabelRow: true,
  failures: [],
  timeClearOfCopy: true,
  timeInsideHeader: true,
} as const;

/** From `ASK_BLOCK_LINE` of header: one line, who asked on the label's line, "Answered by" after
 *  the copy control or under the line. */
const BLOCK_HEADER_LINE = {
  ...BLOCK_HEADER_DRAWN,
  answeredAfterCopy: true,
  askedOnLabelRow: true,
} as const;

/** Below `ASK_BLOCK_LINE` of header: rows, the copy control at the end of the label's, who asked
 *  under it and "Answered by" under that. */
const BLOCK_HEADER_ROWS = {
  ...BLOCK_HEADER_DRAWN,
  answeredUnderAsked: true,
  askedOnLabelRow: false,
  copyAtRightEnd: true,
} as const;

/**
 * The read-only block's header at each width this row reads it at, for this seed. The heights
 * and rows at 1280, 1024 and 768 px are `main`'s at c69be928, before the header drew a state
 * label: this seed, its page clock pinned as this row pins it, counted as this row counts rows,
 * on a build of that tree served by its own harness. There it was 80 px in three rows in the
 * 491 px column 1280 px leaves beside the margin (the urgency and `Decision`; who asked, when
 * and the copy control; who answered), 44 px in one row in the 939 px column at 1024, and 68
 * px in two in the 683 px column at 768 - below 1280 px every button is a 44 px box
 * (`web/src/styles.css`), the copy control with them. A label that made the header taller
 * there, or pushed "Answered by" to a row of its own at 1024, fails it. A phone's 305 px header
 * is held to its rows alone: `main` wrapped who asked onto two lines there, 112 px to these
 * rows' 92.
 *
 * The time is the one part of this seed whose width moves, so the row pins it: at 1024 the one
 * row leaves 70 px to spare on `main` and 7 px here, and a time as wide as "5 minutes ago"
 * puts "Answered by" on a row of its own on both. The family the text is drawn in is pinned
 * too: the browser image the suite runs in answers `sans-serif` as DejaVu Sans, as the runner
 * did (`.github/docker/browser.Dockerfile`), and so do the machines these figures were read on.
 */
const BLOCK_HEADER_AT: Readonly<Record<number, Record<string, unknown>>> = {
  390: { ...BLOCK_HEADER_ROWS, rows: 3 },
  768: { ...BLOCK_HEADER_LINE, height: 68, rows: 2 },
  1024: { ...BLOCK_HEADER_LINE, height: 44, rows: 1 },
  1280: { ...BLOCK_HEADER_ROWS, height: 80, rows: 3 },
};

test("a read-only decision block's header is one wrapping row where its line fits and rows of their own where it does not, as tall as main's from 768 to 1280 px @phone", async ({
  browser,
}) => {
  const { answeredAt, issue } = await seedAnsweredBlock();
  await patchIssue(issue.key, { status: "done" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The page's `Date` is held at the moment the server says the block was answered, its
    // timers left running, so both of the header's times read "just now" however long the row
    // takes, whatever the page's clock says against the server's: the width of each time is
    // part of what the row measures (`BLOCK_HEADER_AT`).
    await page.clock.setFixedTime(answeredAt);
    await page.goto(`/issues/${issue.key}`);
    const header = documentEditor(page).locator(
      '[data-dispatch-ask-block="decide"] header[data-ask-header]'
    );
    await expect(header.locator("[data-dispatch-ask-answered]")).toBeVisible();
    await expect(header.locator("time")).toHaveText(["just now", "just now"]);
    const viewport = page.viewportSize();
    if (viewport === null) throw new Error("the page has no viewport");
    // The header picks its layout by its own width, not the viewport's. A desktop project reads
    // it from 1280 down to a phone's 390 px, a phone project at its own width.
    const widths = viewport.width < 768 ? [viewport.width] : [1280, 1024, 768, 390];
    for (const width of widths) {
      const expected = BLOCK_HEADER_AT[width];
      if (expected === undefined) throw new Error(`no layout is written down for ${width} px`);
      await page.setViewportSize({ height: viewport.height, width });
      await expect
        .poll(() => blockLayout(header), {
          message: `the block header's rows at ${width} px`,
        })
        .toMatchObject(expected);
    }
    // A name the row cuts is still there whole, in the tooltip of the part that truncates.
    await expect(header.locator("[data-dispatch-ask-asked] > [title]")).toHaveAttribute(
      "title",
      new RegExp(`^${planner.title} · `)
    );
  } finally {
    await alice.close();
  }
});

test("a closed issue's answered block in the narrowest reading pane draws its time clear of its copy control, and the control inside its header", async ({
  browser,
}) => {
  const { blockAsk, issue } = await seedAnsweredBlock();
  // The asker's reply under the reader's answer keeps the ask's thread in the reader's Inbox as
  // a Reply row after the close, and the pane opens a decision block's thread in its document.
  await createComment(issue.key, { ask_id: blockAsk.id, body: "Left it is." }, asker);
  await patchIssue(issue.key, { status: "done" });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The pane shows from 1280 px (`READING_PANE_QUERY`), where its narrowest is narrowest.
    await page.setViewportSize({ height: 900, width: 1280 });
    await page.goto("/");
    const row = page.locator(`[data-inbox-row="${replyRowId(blockAsk.id)}"]`);
    await row.locator("time").first().click();
    await expect(row).toHaveAttribute("data-inbox-selected", "true");
    // The reader drags the line between the list and the pane as far right as it goes, where
    // the pane keeps 30% of the two columns (`PaneDivider`): about 266 px.
    const divider = page.getByTestId("inbox-pane-divider");
    await expect(divider).toBeVisible();
    // The wait above drew the divider, and a drawn element has a box.
    const line = await divider.boundingBox();
    if (line === null) throw new Error("the Inbox draws no pane divider");
    const y = line.y + line.height / 2;
    await page.mouse.move(line.x + line.width / 2, y);
    await page.mouse.down();
    await page.mouse.move(1279, y, { steps: 5 });
    await page.mouse.up();
    await expect(divider).toHaveAttribute("aria-valuenow", "30");
    const header = inboxPane(page).locator(
      '[data-dispatch-ask-block="decide"] header[data-ask-header]'
    );
    await expect(header.locator("[data-dispatch-ask-answered]")).toBeVisible();
    await expect
      .poll(() => blockLayout(header), {
        message: "the block header in the narrowest reading pane",
      })
      .toMatchObject(BLOCK_HEADER_ROWS);
  } finally {
    await alice.close();
  }
});

/** The session whose ask the card rows leave waiting on the reader, under a name short enough to
 *  draw whole on a phone's line. */
const shortPlanner: FakeSession = { ...planner, title: "Planner" };

/** The long-titled session whose ask the card rows hand back to it: its name is the turn an
 *  agent holds, the one part of the card's line that gives up width. */
const ledger: FakeSession = {
  ...planner,
  dir: "/workspaces/ledger",
  session_id: sessionIds.tester,
};

/** Two anchored asks on one issue's spec: the short-titled planner's, waiting on the reader, and
 *  the long-titled session's, whose turn the reader's question handed back to it. */
async function seedTurns() {
  await setLiveSessions([shortPlanner, ledger]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Specification\n\nThe mirror replays the ledger on a cold start.\n",
    title: "Copy control",
  });
  const yours = await createAsk(
    issue.key,
    {
      anchor: { artifact: "spec", quote: "replays the ledger" },
      question: "Does a cold start replay the ledger?",
    },
    asker
  );
  const theirs = await createAsk(
    issue.key,
    {
      anchor: { artifact: "spec", quote: "cold start" },
      question: "Which mirror keeps the budget?",
    },
    { actor: { id: ledger.session_id, kind: "session" }, as: "agent" }
  );
  await createComment(issue.key, { ask_id: theirs.id, body: "Which mirror do you mean?" });
  await expect
    .poll(() => getAsk(theirs.id), { intervals: [50, 50, 100, 200, 500] })
    .toMatchObject({ ask: { waiting_on: "agent" } });
  return {
    issue,
    turns: [
      { ask: yours, asker: shortPlanner, turn: "Waiting on you" },
      { ask: theirs, asker: ledger, turn: `Waiting on ${ledger.title}` },
    ],
  };
}

/** An ask card's own line at the width its host draws it, read as every sweep reads it: the
 *  width of its `@container` row, the one condition that picks its layout (`CardAskHeader`: one
 *  line from `ASK_CARD_LINE`, two lines beside the copy control from `ASK_CARD_TWO_LINES`, the
 *  phone's lines below that), the viewport's width, which no longer picks it and which a row
 *  reads only to say which reader it stands for, whether the copy control shares the label's
 *  line, whether who asked and when opens a line of its own under that one, and whether the
 *  asker's name is drawn whole. The row and who asked are read by their own marks, so a row that
 *  lost either says which. */
async function cardLine(card: Locator, name: string) {
  await expect(card.locator("[data-ask-header] [data-copy-ref] button")).toBeVisible();
  const viewport = card.page().viewportSize();
  if (viewport === null) throw new Error("the page has no viewport");
  const drawn = cardHeader({ anchored: true, notched: false, turn: true });
  const reading = await readAtHostWidth(card.locator("[data-ask-header]").first(), {
    ...drawn,
    fields: { ...drawn.fields, name: `[data-ask-card-asked] [title="${name}"]` },
  });
  const asked = drawnField(reading, "asked");
  const copy = drawnField(reading, "copy");
  const label = drawnField(reading, "label");
  const asker = drawnField(reading, "name");
  const { row } = reading;
  if (row === null) throw new Error(`the card draws no row: ${reading.failures.join("; ")}`);
  return {
    askerOwnLine:
      firstLine(asked) > Math.max(lastLine(label), lastLine(copy)) &&
      Math.abs(asked.span.left - row.left) <= 1,
    askerWhole: asker.drawn >= asker.whole - 0.5,
    copyOnLabelLine: copy.lines.includes(firstLine(label)),
    failures: reading.failures,
    row: row.right - row.left,
    viewport: viewport.width,
  };
}

test("the Inbox card's copy control ends the label's one line, whoever's turn it is", async ({
  browser,
}) => {
  const { turns } = await seedTurns();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    for (const { ask, asker: author, turn } of turns) {
      const card = await askCard(page, ask.id);
      await expect(card.getByTestId(`turn-${ask.id}`)).toContainText(turn);
      const line = await cardLine(card, `${author.title} · ${author.session_id}`);
      expect(line.failures, `${turn}: the Inbox card's header at its own width`).toEqual([]);
      // The card's widest layout: a row of `ASK_CARD_LINE` or more, where label, turn, asker and
      // copy control are one line.
      expect(line.row, `${turn}: the Inbox card's row is 27.5rem or wider`).toBeGreaterThanOrEqual(
        ASK_CARD_LINE
      );
      expect(line.copyOnLabelLine, `${turn}: the copy control is on the label's line`).toBe(true);
    }
  } finally {
    await alice.close();
  }
});

test("the margin card's copy control stands beside its two lines, on the label's, whoever's turn it is", async ({
  browser,
}) => {
  const { issue, turns } = await seedTurns();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}`);
    await page.getByRole("tab", { name: "Spec" }).click();
    for (const { ask, asker: author, turn } of turns) {
      const card = marginCard(page, ask.id);
      await expect(card.getByTestId(`turn-${ask.id}`)).toContainText(turn);
      const line = await cardLine(card, `${author.title} · ${author.session_id}`);
      expect(line.failures, `${turn}: the margin card's header at its own width`).toEqual([]);
      // The margin: a row from `ASK_CARD_TWO_LINES` to under `ASK_CARD_LINE`, which breaks the
      // label and turn from the asker and stands the copy control beside both lines. The
      // viewport check no longer picks that layout; it says this row reads the margin as a
      // desktop reader does, at `md` or wider.
      expect(line.viewport, `${turn}: the viewport is md or wider`).toBeGreaterThanOrEqual(768);
      expect(line.row, `${turn}: the margin card's row is 19.5rem or wider`).toBeGreaterThanOrEqual(
        ASK_CARD_TWO_LINES
      );
      expect(line.row, `${turn}: the margin card's row is under 27.5rem`).toBeLessThan(
        ASK_CARD_LINE
      );
      expect(line.copyOnLabelLine, `${turn}: the copy control is on the label's line`).toBe(true);
    }
  } finally {
    await alice.close();
  }
});

/** The answer box of `card` as it is drawn before it holds anything: its content box's height,
 *  read from its border box, and its line height, both in px. */
async function answerBox(card: Locator): Promise<{ content: number; line: number }> {
  return card.locator("[data-ask-answer]").evaluate((box) => {
    const style = getComputedStyle(box);
    const edges = ["borderTopWidth", "borderBottomWidth", "paddingTop", "paddingBottom"] as const;
    const around = edges.reduce((sum, edge) => sum + Number.parseFloat(style[edge]), 0);
    return {
      content: box.getBoundingClientRect().height - around,
      line: Number.parseFloat(style.lineHeight),
    };
  });
}

test("on a phone the Inbox card's copy control ends the label's line, the asker has the next line whole and the answer box is two lines tall, whoever's turn it is @phone", async ({
  browser,
}) => {
  const { turns } = await seedTurns();
  const alice = await asUser(browser, "alice", devices["iPhone 13"]);
  try {
    const page = await alice.newPage();
    await page.goto("/");
    for (const { ask, asker: author, turn } of turns) {
      const card = await askCard(page, ask.id);
      await expect(card.getByTestId(`turn-${ask.id}`)).toContainText(turn);
      const title = `${author.title} · ${author.session_id}`;
      const line = await cardLine(card, title);
      expect(line.failures, `${turn}: the phone card's header at its own width`).toEqual([]);
      // A phone: a row under `ASK_CARD_TWO_LINES`, where the copy control ends the label's line
      // and the asker has the next line to itself. The viewport check no longer picks that
      // layout; it says this row reads the card as a phone does, under `md`, where the copy
      // control and the live link are 44 px touch boxes.
      expect(line.viewport, `${turn}: the viewport is under md`).toBeLessThan(768);
      expect(line.row, `${turn}: the phone card's row is under 19.5rem`).toBeLessThan(
        ASK_CARD_TWO_LINES
      );
      expect(line.copyOnLabelLine, `${turn}: the copy control is on the label's line`).toBe(true);
      expect(line.askerOwnLine, `${turn}: the asker opens the next line`).toBe(true);
      await expect(card.locator(`[data-ask-header] [title="${title}"]`)).toHaveText(author.title);
      // A name that fits that line is drawn whole. The agent's own title does not fit it at
      // all - 223 px of name beside the live link and the time, which leave it 160 - so it
      // truncates there whatever the copy control does, and its tooltip holds it whole.
      if (author === ledger) continue;
      expect(line.askerWhole, `${turn}: the asker's name is not cut short`).toBe(true);
    }
    // The answer box draws two of its lines, its placeholder's two at a phone's width: the touch
    // floor's 44 px (`web/src/styles.css`) is one line, which cut the placeholder mid-line.
    for (const { ask, turn } of turns) {
      const box = await answerBox(await askCard(page, ask.id));
      expect(box.line, `${turn}: the answer box has no line height in px`).toBeGreaterThan(0);
      expect(
        box.content,
        `${turn}: the answer box is under two of its lines tall`
      ).toBeGreaterThanOrEqual(2 * box.line - 0.5);
    }
  } finally {
    await alice.close();
  }
});
