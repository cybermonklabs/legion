import { expect, type Locator, type Page, test } from "@playwright/test";

import { type FakeSession, getSentMessages, setLiveSessions } from "./agents";
import { publishAgentStreamFrame } from "./api";
import { frontLiveView } from "./live-views";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * Completing a session's command at the text cursor (INFRA-812, R5; the browser cases of the
 * Verification document's k7, dispatch://INFRA-812/artifact/verification-md).
 *
 * The menu belongs to the `/` token the cursor sits in, wherever that token is: a pick at the
 * start of the message is the command line the session runs, and a pick inside a sentence is
 * the person naming a skill to the session, so it writes the bare name. A `/` inside a word is
 * part of that word - a path, a date - and opens nothing. Every row of the menu carries its
 * whole description, because a row of this list is read to learn what the command does.
 *
 * The `commands` frame is spelt here as the wire carries it, never imported from the SPA or the
 * contracts, so this file reads the same against the commit before the change and each row
 * fails there on its own assertion.
 */

/** The session these rows read: a UUID, which Dispatch's agent routes require, and this file's
 *  own, since the relay's replay is process-global. */
const COMPLETION_ID = "01a0e812-0005-7000-8000-000000000001";

/** The skill a pick writes, and the one name of the list that holds `asd`. */
const SKILL = "skill:asd-ste100";

/** A description longer than a row of the menu is wide, at a laptop width and at a phone's: the
 *  row has to carry it on more than one line, and cut none of it. It is under the 200 characters
 *  one `commands` frame may carry for a description. */
const LONG_DESCRIPTION =
  "Use when English text must be parsed without a human to resolve ambiguity: a tool description, a specification, a prompt, or any other instruction that another agent or a program has to read.";

/**
 * Twelve commands, as a session that runs typed input lists them: three skills, with the one
 * this file picks first among them, and the built-ins and prompts around them. `asd` holds for
 * one name alone and `skil` for the three skills, so a pick made after either is the same
 * command, and the menu is long enough that the reader scrolls rather than reads it whole.
 */
const COMMANDS = [
  { description: "Compact the session's context", name: "compact", source: "builtin" },
  { description: "Switch the session's model", name: "model", source: "builtin" },
  { description: "Set the thinking level", name: "effort", source: "builtin" },
  { description: LONG_DESCRIPTION, name: SKILL, source: "skill" },
  { description: "Write to Dispatch as this session", name: "skill:dispatch", source: "skill" },
  { description: "Say it in plain words", name: "skill:plain-words", source: "skill" },
  { description: "Write the day's report", name: "daily-report", source: "prompt" },
  { description: "Review the open pull request", name: "review", source: "prompt" },
  { description: "List what this session can run", name: "help", source: "builtin" },
  { description: "Show the session's token usage", name: "usage", source: "builtin" },
  { description: "Start a new session", name: "new", source: "builtin", terminalOnly: true },
  { description: "Resume an older session", name: "resume", source: "builtin", terminalOnly: true },
];

/** The live session whose commands the composer completes. `last_seen` is taken when the row
 *  seeds, never at this module's scope, which a worker evaluates once and every later spec of
 *  that worker ages. */
function completionSession(): FakeSession {
  return {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/completion",
    last_seen: Date.now() - 5_000,
    machine_id: "completion-host",
    roles: [],
    session_id: COMPLETION_ID,
    title: "Completer",
  };
}

/** Seeds the session and the list its replay answers a viewer with. */
async function seedCommands(): Promise<void> {
  await setLiveSessions([completionSession()]);
  await publishAgentStreamFrame(
    COMPLETION_ID,
    {
      frames: [{ commands: COMMANDS, kind: "commands", seq: 1, v: 1 }],
      session_id: COMPLETION_ID,
      v: 1,
    },
    "replay"
  );
}

/** The message box of the view the reader is looking at: a page-wide locator would also match
 *  the view lingering behind it. */
function composer(page: Page): Locator {
  return frontLiveView(page).getByTestId("agent-composer").locator("textarea");
}

/** The open menu of commands. */
function commandMenu(page: Page): Locator {
  return frontLiveView(page).getByRole("listbox", { name: "Slash commands" });
}

/** Opens the live view with the box focused and the list in place, and answers the box. */
async function openComposer(page: Page): Promise<Locator> {
  await page.goto(`/agents/${COMPLETION_ID}/live`);
  const input = composer(page);
  await expect(input).toHaveAttribute("placeholder", /\/ for commands/);
  await input.click();
  return input;
}

test.beforeEach(async () => {
  await resetDatabase();
});

// R5. The menu belongs to the token the cursor sits in, not to the start of the message: a
// person who writes a sentence and names a skill in it gets the list for that `/`, and the pick
// leaves the sentence reading as they meant it - the bare name, with no slash and none of the
// publisher's `skill:` prefix, because nothing in a sentence is run.
// SLOW(>=2s): a live view opened on the session's replay, then the menu driven from the
// keyboard - each keystroke is a search of the session's list and a redraw of the popover.
test("Tab inside a sentence completes the skill as its bare name", async ({ browser }) => {
  await seedCommands();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    const input = await openComposer(page);
    await input.fill("please read /asd");
    await expect(commandMenu(page).getByRole("option")).toHaveText([new RegExp(`^/${SKILL}`)]);

    await input.press("Tab");
    await expect(input).toHaveValue("please read asd-ste100 ");
    await expect(commandMenu(page)).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// R5. A line break starts a token as a space does, so the second line of a message completes
// like the first.
// SLOW(>=2s): a live view opened on the session's replay, then the menu driven from the
// keyboard.
test("Tab completes a skill named after a line break", async ({ browser }) => {
  await seedCommands();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    const input = await openComposer(page);
    await input.fill("Before you start:\n/asd");
    await expect(commandMenu(page).getByRole("option")).toHaveText([new RegExp(`^/${SKILL}`)]);

    await input.press("Tab");
    await expect(input).toHaveValue("Before you start:\nasd-ste100 ");
  } finally {
    await context.close();
  }
});

// R5. A `/` inside a word belongs to that word - a path, a date - and opens nothing. The same
// box, one space later, opens the menu: the silence is the rule and not a box that completes
// nothing.
// SLOW(>=2s): a live view opened on the session's replay, then the box rewritten twice and the
// menu waited for each time.
test("a slash inside a word opens no menu", async ({ browser }) => {
  await seedCommands();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    const input = await openComposer(page);
    await input.fill("see docs/asd");
    await expect(commandMenu(page)).toHaveCount(0);

    await input.fill("see docs /asd");
    await expect(commandMenu(page).getByRole("option")).toHaveText([new RegExp(`^/${SKILL}`)]);
  } finally {
    await context.close();
  }
});

// R5. Where the `/` sits decides what a pick is. A pick that starts the message is a command
// line: the session runs it as the person would have typed it at its own terminal, with the
// slash and the publisher's own name, and Send hands it over as it stands. The same pick made
// inside a sentence is the person naming the skill, and what the session receives is that
// sentence - so Send runs a command only where the message is one.
// SLOW(>=2s): a live view opened on the session's replay, two picks made from the keyboard, and
// both sends read back from the fake Envoy.
test("Tab at the start of a message completes the command, and Send runs it as one", async ({
  browser,
}) => {
  await seedCommands();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    const input = await openComposer(page);
    await input.fill("/skil");
    await expect(commandMenu(page).getByRole("option").first()).toHaveText(
      new RegExp(`^/${SKILL}`)
    );

    await input.press("Tab");
    await expect(input).toHaveValue(`/${SKILL} `);

    await input.press("Enter");
    await expect
      .poll(async () => (await getSentMessages()).map((sent) => sent.message))
      .toContain(`/${SKILL}`);

    // The same skill named inside a sentence: the session is told about it, never asked to run
    // it, so nothing carrying a slash goes out.
    await expect(input).toHaveValue("");
    await input.fill("please read /skil");
    await input.press("Tab");
    await expect(input).toHaveValue("please read asd-ste100 ");

    await input.press("Enter");
    await expect
      .poll(async () => (await getSentMessages()).map((sent) => sent.message))
      .toEqual([`/${SKILL}`, "please read asd-ste100"]);
  } finally {
    await context.close();
  }
});

// R5. One list, wherever the `/` is: the session's commands do not change because the token sits
// in a sentence rather than at the start of the message.
// SLOW(>=2s): a live view opened on the session's replay, then the whole list read twice, once
// for each place the `/` sits in.
test("the menu offers the same list at the start of a message and inside a sentence", async ({
  browser,
}) => {
  await seedCommands();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    const input = await openComposer(page);
    await input.fill("/");
    await expect(commandMenu(page).getByRole("option")).toHaveCount(COMMANDS.length);
    const atStart = await commandMenu(page).getByRole("option").allInnerTexts();

    await input.fill("please read /");
    await expect(commandMenu(page).getByRole("option")).toHaveCount(COMMANDS.length);
    expect(await commandMenu(page).getByRole("option").allInnerTexts()).toEqual(atStart);
  } finally {
    await context.close();
  }
});

// R5. A row of this list is read to learn what the command does, so a description longer than
// the row is wide is carried on as many lines as it takes and cut nowhere. The claim is read
// off the page - how many lines the text is set on, and whether anything of it is clipped - so
// it runs at both widths, since a narrow row is where a description runs out of room.
// SLOW(>=2s): a live view opened on the session's replay, then the menu's longest row measured
// line by line.
test("a description longer than its row is shown whole, on more than one line @phone", async ({
  browser,
}) => {
  await seedCommands();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    const input = await openComposer(page);
    await input.fill("/");
    const options = commandMenu(page).getByRole("option");
    await expect(options).toHaveCount(COMMANDS.length);

    const row = options.filter({ hasText: `/${SKILL}` });
    await expect(row).toHaveCount(1);
    const description = row.locator(":scope > span").nth(1);
    await expect(description).toHaveText(LONG_DESCRIPTION);

    const shown = await description.evaluate((span) => {
      const style = getComputedStyle(span);
      const range = document.createRange();
      range.selectNodeContents(span);
      return {
        clientWidth: span.clientWidth,
        // One rectangle per line the text is set on.
        lines: range.getClientRects().length,
        scrollWidth: span.scrollWidth,
        textOverflow: style.textOverflow,
        whiteSpace: style.whiteSpace,
      };
    });
    expect(shown.lines).toBeGreaterThan(1);
    expect(shown.textOverflow).not.toBe("ellipsis");
    expect(shown.whiteSpace).not.toBe("nowrap");
    // Nothing of it is held outside the row it is drawn in, so nothing of it is cut.
    expect(shown.scrollWidth).toBeLessThanOrEqual(shown.clientWidth);
  } finally {
    await context.close();
  }
});
