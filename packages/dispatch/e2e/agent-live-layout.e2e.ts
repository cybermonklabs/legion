import { expect, type Locator, type Page, test } from "@playwright/test";

import { type FakeSession, setLiveSessions } from "./agents";
import { getAgentStates, publishAgentStreamFrame } from "./api";
import { frontLiveView } from "./live-views";
import { resetDatabase } from "./seed";
import { assertPageFits } from "./unclipped";
import { asUser } from "./users";

/**
 * Where the live view puts the session's facts and its controls (INFRA-812, R6; the browser
 * cases of the Verification document's k10, dispatch://INFRA-812/artifact/verification-md).
 *
 * The header carries the way back, the session's name with its running mark, the live state and
 * the pin, and nothing else: no picker, no machine, no directory and no paragraph of prose. One
 * row under the message box carries the rest - what a message is delivered as, where the session
 * runs, what it runs on, its model, its thinking level, how full its window is and the one
 * question mark that holds the rules the page used to print - on one line at 1440 pixels, with a
 * long directory the one part that shortens. The line under the box says nothing until a press
 * is refused, and at a phone width the row scrolls sideways under a ring and a question mark
 * that keep the box's right edge.
 *
 * The frames are spelt here as the wire carries them, never imported from the SPA or the
 * contracts, so the file runs unchanged against the commit before the change and each row fails
 * there on its own assertion.
 */

/** The sessions these rows read, by id. Each is a UUID, which Dispatch's agent routes require,
 *  and each is this file's own: the relay's replay is process-global, so a session another spec
 *  seeded would carry that spec's frames into these rows. */
const HEADER_ID = "01a0e812-0a06-7000-8000-000000000001";
const ROW_ID = "01a0e812-0a06-7000-8000-000000000002";
const HINT_ID = "01a0e812-0a06-7000-8000-000000000003";
const HELP_ID = "01a0e812-0a06-7000-8000-000000000004";
const PHONE_ID = "01a0e812-0a06-7000-8000-000000000005";

const MACHINE = "layout-host";

/** A directory no row of this page can show whole: it is what the row shortens, and what the
 *  header must not carry at all. */
const LONG_DIR =
  "/workspaces/legion/packages/dispatch/web/src/features/agent-view/under-the-message-box/the-long-way-round";

/** A live session for a row to open. `last_seen` is taken when the row seeds, never at this
 *  module's scope, which a worker evaluates once and every later spec of that worker ages. */
function session(sessionID: string, title: string): FakeSession {
  return {
    capabilities: ["aside", "btw", "steer"],
    dir: LONG_DIR,
    last_seen: Date.now() - 5_000,
    machine_id: MACHINE,
    roles: [],
    session_id: sessionID,
    title,
  };
}

/** Everything this file's sessions report before a reader opens them, as the wire carries it:
 *  one settled turn, the model and thinking level the two pickers switch, the session's own
 *  commands - without which those settings are text rather than pickers, since the switch is a
 *  command the session runs - and how full its window is, which is the ring in the row. */
function replayOf(sessionID: string): Record<string, unknown> {
  return {
    frames: [
      {
        kind: "message",
        message: {
          at: 1_000,
          id: "m1",
          parts: [{ text: "Reading the release notes.", type: "text" }],
          role: "assistant",
          streaming: false,
        },
        seq: 1,
        v: 1,
      },
      {
        kind: "settings",
        model: "anthropic/model-a",
        models: ["anthropic/model-a", "anthropic/model-b"],
        reported: true,
        seq: 2,
        thinking: { level: "medium", levels: ["low", "medium", "high"] },
        v: 1,
      },
      {
        commands: [
          { description: "Switch the session's model", name: "model", source: "builtin" },
          { description: "Set the thinking level", name: "effort", source: "builtin" },
        ],
        kind: "commands",
        seq: 3,
        v: 1,
      },
      { contextWindow: 200_000, kind: "context", seq: 4, tokens: 68_000, v: 1 },
    ],
    session_id: sessionID,
    v: 1,
  };
}

function livePath(sessionID: string): string {
  return `/agents/${sessionID}/live`;
}

/** The one row under the message box. */
function footer(page: Page): Locator {
  return frontLiveView(page).getByTestId("agent-composer-footer");
}

/** The part of that row that scrolls sideways at a phone width: the ring and the question mark
 *  sit beside it, not in it. */
function facts(page: Page): Locator {
  return footer(page).getByTestId("agent-composer-facts");
}

interface Box {
  bottom: number;
  height: number;
  left: number;
  right: number;
  top: number;
}

/** One element's box on screen, rounded to whole pixels. */
async function boxOf(locator: Locator): Promise<Box> {
  const box = await locator.boundingBox();
  if (box === null) throw new Error("the element is not laid out");
  return {
    bottom: Math.round(box.y + box.height),
    height: Math.round(box.height),
    left: Math.round(box.x),
    right: Math.round(box.x + box.width),
    top: Math.round(box.y),
  };
}

/** Whether the element's own text is cut to fit the width it was given. */
function clipped(locator: Locator): Promise<boolean> {
  return locator.evaluate((element) => element.scrollWidth > element.clientWidth + 1);
}

test.beforeEach(async () => {
  await resetDatabase();
});

// R6. The header is the way back, who this is, whether the conversation is live, and the pin -
// and nothing else. What the header carried before the change is read for by name: a picker, the
// machine, the directory, and the paragraph of prose that stood under it. The pin is the
// person's, so the press is read back from Dispatch's own answer rather than from the control
// alone.
// SLOW(>=2s): a live view opened on the session's replay, then a pin and an unpin each waited on
// until Dispatch answers.
test("the live view's header holds the back link, the name, the live state and a pin, and nothing else", async ({
  browser,
}) => {
  await setLiveSessions([session(HEADER_ID, "Planner")]);
  await publishAgentStreamFrame(HEADER_ID, replayOf(HEADER_ID), "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(livePath(HEADER_ID));
    const head = frontLiveView(page).getByTestId("agent-view-header");
    await expect(head).toBeVisible();

    await expect(head.getByRole("link", { name: "← Agents" })).toHaveAttribute("href", "/agents");
    await expect(head.getByRole("heading", { level: 1 })).toHaveText("Planner");
    // The name carries the session's running mark, as every surface draws it.
    await expect(head.locator("[data-agent-name]")).toHaveAttribute("data-agent-state", "running");
    await expect(head.getByRole("img", { exact: true, name: "Running" })).toBeVisible();
    // Whether the conversation itself is reaching the page.
    await expect(head).toContainText("Live");

    // What the header carried before this change, now under the message box: the row there holds
    // all three, and the header holds none of them.
    await expect(head.getByRole("combobox")).toHaveCount(0);
    await expect(head.getByTestId("agent-footer-machine")).toHaveCount(0);
    await expect(head.getByTestId("agent-footer-dir")).toHaveCount(0);
    await expect(head).not.toContainText(MACHINE);
    await expect(head).not.toContainText("/workspaces/legion");
    await expect(footer(page).getByTestId("agent-footer-machine")).toHaveText(MACHINE);

    // No paragraph of the page's own prose stands under the header: the rules it used to print
    // are behind the question mark, and the only paragraphs this page has left are a failure's
    // and a switch's, neither of which is on screen at rest.
    await expect(frontLiveView(page).getByTestId("agent-conversation").locator("> p")).toHaveCount(
      0
    );

    // The pin pins and unpins, and what it wrote is the person's own state, not this page's.
    await head.getByRole("button", { exact: true, name: "Pin Planner" }).click();
    const unpin = head.getByRole("button", { exact: true, name: "Unpin Planner" });
    await expect(unpin).toHaveAttribute("aria-pressed", "true");
    await expect
      .poll(async () => (await getAgentStates())[HEADER_ID]?.pinned_at !== undefined)
      .toBe(true);
    await unpin.click();
    await expect(head.getByRole("button", { exact: true, name: "Pin Planner" })).toHaveAttribute(
      "aria-pressed",
      "false"
    );
    await expect.poll(async () => (await getAgentStates())[HEADER_ID]?.pinned_at).toBeUndefined();
  } finally {
    await context.close();
  }
});

// R6. One row under the message box holds the seven parts, and at a laptop width it is one line:
// every part of it shares that line, and the directory - the one part that stretches - is what
// gives way when there is not room for all of it. The width is set here for every project, so
// this row reads the same claim wherever it runs and carries no phone tag.
// SLOW(>=2s): a live view opened on the session's replay, then every part of the row measured
// on screen.
test("the row under the message box holds the facts and the controls on one line at 1440, shortening the directory first", async ({
  browser,
}) => {
  await setLiveSessions([session(ROW_ID, "Builder")]);
  await publishAgentStreamFrame(ROW_ID, replayOf(ROW_ID), "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ height: 900, width: 1440 });
    await page.goto(livePath(ROW_ID));
    const row = footer(page);
    await expect(row).toBeVisible();

    const parts = {
      delivery: row.getByTestId("agent-delivery-picker"),
      dir: row.getByTestId("agent-footer-dir"),
      help: row.getByTestId("agent-help"),
      machine: row.getByTestId("agent-footer-machine"),
      model: row.getByTestId("agent-model-picker"),
      ring: row.getByTestId("agent-context-ring"),
      thinking: row.getByTestId("agent-thinking-picker"),
    };
    for (const part of Object.values(parts)) {
      await expect(part).toBeVisible();
    }
    // The ring and the question mark sit beside the scrolling facts, not inside them, so they
    // keep the box's right edge at every width (the phone row below reads that).
    await expect(facts(page).getByTestId("agent-context-ring")).toHaveCount(0);
    await expect(facts(page).getByTestId("agent-help")).toHaveCount(0);

    // One line: every part overlaps every other vertically, so none of them wrapped under
    // another, and the row is no taller than its tallest part.
    const boxes = await Promise.all(Object.values(parts).map(boxOf));
    const top = Math.max(...boxes.map((box) => box.top));
    const bottom = Math.min(...boxes.map((box) => box.bottom));
    expect(bottom, `the row wrapped: ${JSON.stringify(boxes)}`).toBeGreaterThan(top);
    const rowBox = await boxOf(row);
    expect(rowBox.height).toBeLessThanOrEqual(Math.max(...boxes.map((box) => box.height)) + 2);

    // The directory is the one part that gives way: its text is cut to the width the rest of the
    // line left it, with the whole path one hover away, and nothing else on the row is cut.
    const dirText = parts.dir.locator("span").first();
    expect(await clipped(dirText)).toBe(true);
    await expect(dirText).toHaveAttribute("title", LONG_DIR);
    for (const part of [parts.machine, parts.delivery, parts.model, parts.thinking]) {
      expect(await clipped(part), `${await part.getAttribute("data-testid")} was cut`).toBe(false);
    }
  } finally {
    await context.close();
  }
});

// R6. The line under the box is the Send control's own words and nothing else: empty while
// nobody has pressed anything, and the reason once a press was refused, which Send still names
// as its description for a reader who cannot see the line. How to send is no longer printed
// there: that is a rule of the page, which the question mark holds.
// SLOW(>=2s): a live view opened on the session's replay before the box is pressed and read.
test("the line under the box reads nothing at rest and carries a refused press's reason", async ({
  browser,
}) => {
  await setLiveSessions([session(HINT_ID, "Scribe")]);
  await publishAgentStreamFrame(HINT_ID, replayOf(HINT_ID), "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(livePath(HINT_ID));
    const composer = frontLiveView(page).getByTestId("agent-composer");
    const hint = composer.getByTestId("agent-composer-hint");
    const send = composer.getByRole("button", { exact: true, name: "Send" });
    await expect(send).toBeVisible();
    await expect(hint).toHaveText("");
    await expect(hint).not.toContainText("Enter");

    await send.click({ force: true });
    await expect(hint).toHaveText("Type a message first.");
    await expect(send).toHaveAccessibleDescription("Type a message first.");
    // `toBeVisible` passes for a 1 px line, so the height is the check that a reader sees it.
    expect((await hint.boundingBox())?.height ?? 0).toBeGreaterThan(8);

    // Typing takes the refusal away again, and the line goes back to saying nothing.
    await composer.locator("textarea").fill("status?");
    await expect(hint).toHaveText("");
  } finally {
    await context.close();
  }
});

// R6. The question mark is where the three rules the page used to print now live: what `!` does,
// where a command runs, and that the session's turns are relayed while the page is open and not
// stored, while the messages sent through Dispatch and its replies are kept. Escape closes it.
// SLOW(>=2s): a live view opened on the session's replay before the panel is opened and read.
test("the question mark opens a panel naming what ! does, where a command runs and what is kept", async ({
  browser,
}) => {
  await setLiveSessions([session(HELP_ID, "Helper")]);
  await publishAgentStreamFrame(HELP_ID, replayOf(HELP_ID), "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(livePath(HELP_ID));
    const question = footer(page).getByTestId("agent-help");
    const panel = footer(page).getByTestId("agent-help-popover");
    await expect(question).toHaveAttribute("aria-expanded", "false");
    await expect(panel).toHaveCount(0);

    await question.click();
    await expect(panel).toBeVisible();
    await expect(panel).toContainText("runs the rest as a shell command in this session");
    await expect(panel).toContainText("at the session itself");
    await expect(panel).toContainText("relayed while this page is open and are not stored");
    await expect(panel).toContainText("Messages sent to it through Dispatch, and its Dispatch");
    await expect(panel).toContainText("are kept");

    await page.keyboard.press("Escape");
    await expect(panel).toHaveCount(0);
    await expect(question).toHaveAttribute("aria-expanded", "false");
    await expect(question).toBeFocused();
  } finally {
    await context.close();
  }
});

// R6. At a phone width the facts are wider than the screen, so they scroll sideways inside the
// row while the ring and the question mark keep the box's right edge: a reader's thumb finds
// both wherever the facts have been scrolled to. The page itself never scrolls sideways, at
// either end of that scroll.
// SLOW(>=2s): a live view opened on the session's replay at a phone width, then the row scrolled
// and both edges measured before and after.
test("at a phone width the row scrolls sideways under a ring and a question mark that keep the right edge @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the row under the message box scrolls sideways only below sm"
  );
  await setLiveSessions([session(PHONE_ID, "Pocket")]);
  await publishAgentStreamFrame(PHONE_ID, replayOf(PHONE_ID), "replay");
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(livePath(PHONE_ID));
    const row = footer(page);
    await expect(row).toBeVisible();
    const scroller = facts(page);
    const ring = row.getByTestId("agent-context-ring");
    const question = row.getByTestId("agent-help");

    // The facts are wider than the row gives them, which is what makes the row a scroller.
    expect(await clipped(scroller)).toBe(true);
    const rowBox = await boxOf(row);
    const before = { question: await boxOf(question), ring: await boxOf(ring) };
    expect(before.question.right).toBeLessThanOrEqual(rowBox.right);
    expect(before.ring.right).toBeLessThanOrEqual(before.question.left);
    await assertPageFits(page);

    // Scrolled to the far end of the facts: the delivery mode has gone off the left of the row,
    // and the ring and the question mark have not moved.
    await scroller.evaluate((element) => {
      element.scrollLeft = element.scrollWidth;
    });
    await expect.poll(() => scroller.evaluate((element) => element.scrollLeft)).toBeGreaterThan(0);
    const after = { question: await boxOf(question), ring: await boxOf(ring) };
    expect(after.ring).toEqual(before.ring);
    expect(after.question).toEqual(before.question);
    await assertPageFits(page);
  } finally {
    await context.close();
  }
});
