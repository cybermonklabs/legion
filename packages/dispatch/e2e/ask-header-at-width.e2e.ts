import { devices, expect, type Locator, test } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
import { createAsk, createComment, createIssue, createProject, getAsk, patchIssue } from "./api";
import { type FieldReading, type Reading, type Rect, readAtHostWidth } from "./ask-header-reader";
import {
  ANSWERED_BLOCK,
  ASK_BODY_BESIDE,
  asker,
  CARD_HEADER,
  planner,
  resetToPlanner,
  seedAnsweredBlock,
} from "./ask-header-rows";
import { documentEditor, marginCard } from "./editor";
import { askCard, inboxPane } from "./inbox-pane";
import { replyRowId } from "./inbox-threads";
import { sessionIds } from "./session-ids";
import { asUser } from "./users";

/**
 * INFRA-341, R1: ask headers read at the one width a reader meets each at, beside
 * `e2e/ask-header-sweep.e2e.ts`, which sweeps each header through its own widths. These rows read
 * through the same reader (`readHeader`, `e2e/ask-header-reader.ts`, which holds what every row
 * of both files reads with and no row) a sweep reads each of its widths with: the Inbox card at
 * 1280 px, the margin card beside a 1280 px document, the Inbox card on an iPhone 13, a closed
 * issue's answered block at 1280, 1024, 768 and 390 px, and the same block in the Inbox's
 * narrowest reading pane. Each holds there the five assertions that file's head names, and what
 * that width draws: the layout the row's width picks, the line each part stands on, and the
 * block's height against `main`'s. What the rows of both files share is
 * `e2e/ask-header-rows.ts`'s.
 */

test.beforeEach(resetToPlanner);

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

/** An ask card's first line at the width its host draws it, read as every sweep reads it: the
 *  width of its ask body, the one condition that picks its layout (`CardAskHeader`: the line's
 *  end beside the question's words from `ASK_BODY_BESIDE`, under them below it), the viewport's
 *  width, which does not pick it and which a row reads only to say which reader it stands for,
 *  whether the copy control shares the label's line, and whether the asker's name is drawn
 *  whole. The line's end and who asked are read by their own marks, so a row that lost either
 *  says which. */
async function cardLine(card: Locator, name: string) {
  await expect(card.locator("[data-ask-header] [data-copy-ref] button")).toBeVisible();
  const viewport = card.page().viewportSize();
  if (viewport === null) throw new Error("the page has no viewport");
  const reading = await readAtHostWidth(card.locator("[data-ask-header]").first(), {
    ...CARD_HEADER,
    fields: { ...CARD_HEADER.fields, name: `[data-ask-card-asked] [title="${name}"]` },
  });
  const copy = drawnField(reading, "copy");
  const label = drawnField(reading, "label");
  const asker = drawnField(reading, "name");
  if (reading.row === null) {
    throw new Error(`the card draws no first line's end: ${reading.failures.join("; ")}`);
  }
  return {
    askerWhole: asker.drawn >= asker.whole - 0.5,
    body: reading.width,
    copyOnLabelLine: copy.lines.includes(firstLine(label)),
    failures: reading.failures,
    viewport: viewport.width,
  };
}

test("the Inbox card's copy control is on the label's line, at the end of the first line beside the question's words, whoever's turn it is", async ({
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
      // The card's widest layout: an ask body of `ASK_BODY_BESIDE` or more, where the kind, who
      // asked and when, the label and the copy control are one line beside the question's words.
      expect(line.body, `${turn}: the Inbox card's body is 32rem or wider`).toBeGreaterThanOrEqual(
        ASK_BODY_BESIDE
      );
      expect(line.copyOnLabelLine, `${turn}: the copy control is on the label's line`).toBe(true);
    }
  } finally {
    await alice.close();
  }
});

test("the margin card's copy control is on the label's line, under the question's words, whoever's turn it is", async ({
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
      // The margin: an ask body under `ASK_BODY_BESIDE`, where the first line's end stands under
      // the question's words. The viewport check does not pick that layout; it says this row
      // reads the margin as a desktop reader does, at `md` or wider.
      expect(line.viewport, `${turn}: the viewport is md or wider`).toBeGreaterThanOrEqual(768);
      expect(line.body, `${turn}: the margin card's body is under 32rem`).toBeLessThan(
        ASK_BODY_BESIDE
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

test("on a phone the Inbox card's copy control is on the label's line under the question's words, the asker's name is drawn whole and the answer box is two lines tall, whoever's turn it is @phone", async ({
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
      // A phone: an ask body under `ASK_BODY_BESIDE`, where the first line's end stands under
      // the question's words and each of its parts takes a line of its own before it gives up
      // width. The viewport check does not pick that layout; it says this row reads the card as
      // a phone does, under `md`, where the copy control and the live link are 44 px touch boxes.
      expect(line.viewport, `${turn}: the viewport is under md`).toBeLessThan(768);
      expect(line.body, `${turn}: the phone card's body is under 32rem`).toBeLessThan(
        ASK_BODY_BESIDE
      );
      expect(line.copyOnLabelLine, `${turn}: the copy control is on the label's line`).toBe(true);
      await expect(card.locator(`[data-ask-header] [title="${title}"]`)).toHaveText(author.title);
      // A name that fits its line is drawn whole. The agent's own title does not fit a phone's
      // line beside the live link and the time, so it truncates there whatever the copy control
      // does, and its tooltip holds it whole.
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
