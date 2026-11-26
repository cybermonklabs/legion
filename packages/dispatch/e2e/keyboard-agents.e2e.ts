import {
  agentRow,
  issueListOf,
  issueOption,
  issueSearchIn,
  openAgents,
  plannerMention,
  plannerSession,
  reviewerMention,
  seedAgents,
  shownAgentRows,
} from "./agents";
import { createMessage } from "./api";
import { refPicker } from "./ref-picker";
import { holdPosts, pasteFile, refusePosts, WRITE_REFUSED_TEXT } from "./sends";
import { expect, test } from "./test";
import { asUser } from "./users";

test.describe("agents page", () => {
  // @pr: The Agents page's keyboard surface: j/k over the rows, Enter into the composer, i into the issue picker, x to select and Shift+P to pin.
  test("j/k rove the agent rows, Enter opens the composer, i the issue picker, x selects and Shift+P pins @pr", async ({
    browser,
  }, testInfo) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const rows = shownAgentRows(page);
      await expect(rows).toHaveCount(2);
      const planner = rows.nth(0);
      const reviewer = rows.nth(1);
      const plannerToggle = planner.getByRole("button", { exact: true, name: "Planner" });

      await page.keyboard.press("j");
      await expect(planner).toBeFocused();
      await page.keyboard.press("j");
      await expect(reviewer).toBeFocused();
      await page.keyboard.press("k");
      await expect(planner).toBeFocused();
      await expect(plannerToggle).toHaveAttribute("aria-expanded", "false");

      // Enter expands the collapsed row and hands focus to its composer; letters then type.
      await page.keyboard.press("Enter");
      const composer = planner.getByRole("textbox", { name: "Comment" });
      await expect(plannerToggle).toHaveAttribute("aria-expanded", "true");
      await expect(composer).toBeFocused();
      await page.keyboard.type("j");
      await expect(composer).toHaveValue("j");
      await expect(reviewer).not.toBeFocused();
      const agentsShot = testInfo.outputPath(`agents-keyboard-${testInfo.project.name}.png`);
      await page.screenshot({ fullPage: true, path: agentsShot });
      await testInfo.attach(`agents keyboard (${testInfo.project.name})`, {
        contentType: "image/png",
        path: agentsShot,
      });

      // Escape out of an untouched composer is one level out: back to the row it belongs to.
      await composer.fill("");
      await page.keyboard.press("Escape");
      await expect(planner).toBeFocused();

      await page.keyboard.press("x");
      await expect(
        planner.getByRole("checkbox", { name: "Select Planner for broadcast" })
      ).toBeChecked();

      // i expands a collapsed row too, and opens its issue picker.
      await page.keyboard.press("j");
      await expect(reviewer).toBeFocused();
      const reviewerPicker = reviewer.getByRole("button", { name: "Choose issue" });
      await page.keyboard.press("i");
      await expect(reviewer.getByRole("button", { exact: true, name: "Reviewer" })).toHaveAttribute(
        "aria-expanded",
        "true"
      );
      await expect(reviewerPicker).toHaveAttribute("aria-expanded", "true");
      // `i` leaves the reader in the Issue list's text box, where the letters narrow the issues
      // and are not the page's keys - Escape is the way back to the row, closing it behind them.
      await expect(issueSearchIn(reviewer)).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(reviewerPicker).toHaveAttribute("aria-expanded", "false");
      await expect(reviewer).toBeFocused();

      // Shift+P pins the focused agent, and a pin is the viewer's own remembered preference.
      await page.keyboard.press("k");
      await expect(planner).toBeFocused();
      await page.keyboard.press("Shift+P");
      await expect(planner.getByRole("button", { name: "Unpin Planner" })).toBeVisible();
      await page.reload();
      await expect(
        shownAgentRows(page).nth(0).getByRole("button", { name: "Unpin Planner" })
      ).toBeVisible();

      // `?` lists the new scope: a scope missing from `SCOPE_ORDER` is dropped silently.
      await page.locator("body").focus();
      await page.keyboard.press("?");
      const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
      const agentsSection = help.getByRole("region", { name: "Agents" });
      await expect(agentsSection).toBeVisible();
      await expect(
        agentsSection.getByRole("listitem").filter({ hasText: "Pin or unpin the focused agent" })
      ).toBeVisible();
      // Escape out of a row's composer is real behaviour, so `?` says so: the Inbox, Board and
      // Architecture all list theirs, and a help screen that omits one is a help screen a reader
      // cannot trust.
      await expect(
        agentsSection.getByRole("listitem").filter({ hasText: "Back to the agent row" })
      ).toBeVisible();
    } finally {
      await context.close();
    }
  });

  // `i` has to leave the reader inside what it opened: the Issue list's text box is where the
  // arrows choose an issue and where Escape is the list's own way out. Left on the row, the key
  // would open something no keystroke could then use or close.
  test("i lands in the issue picker, and Escape there closes it and returns to the row", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      await page.keyboard.press("j");
      await page.keyboard.press("i");
      const toggle = row.getByRole("button", { name: "Choose issue" });
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      const picker = issueSearchIn(row);
      await expect(picker).toBeFocused();

      await page.keyboard.press("Escape");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(picker).toHaveCount(0);
      await expect(row).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // A pick is what `i` opened the picker for, so committing one hands the reader back to the
  // message it belongs to rather than dropping them on the document as the list closes.
  test("choosing an issue closes the picker and leaves focus in the composer", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      await page.keyboard.press("j");
      await page.keyboard.press("i");
      const toggle = row.getByRole("button", { name: "Choose issue" });
      await expect(issueSearchIn(row)).toBeFocused();

      // The arrows move the active issue; Enter is the pick.
      await page.keyboard.press("ArrowDown");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await page.keyboard.press("Enter");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(toggle).toContainText("CORE-1");
      await expect(row.getByRole("textbox", { name: "Comment" })).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // The list takes the focus only once its issues land, so the whole latency of that read is a
  // window in which the reader is somewhere else. Focus is the list's to take only if they are
  // still where the open left them.
  test("a slow issue list never pulls the reader off the row they moved to", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      let release: (() => void) | undefined;
      const held = new Promise<void>((resolve) => {
        release = resolve;
      });
      await page.route("**/api/v1/issues?*", async (route) => {
        await held;
        return route.fallback();
      });
      await openAgents(page);
      const rows = shownAgentRows(page);

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await page.keyboard.press("j");
      await expect(rows.nth(1)).toBeFocused();

      release?.();
      // The issues have landed, which is when the list decides whether to take the focus. Its
      // text box is drawn at the open, before they land, so it cannot be what this waits on; the
      // `<select>` the list replaced was drawn only once they had, which is what this row waited
      // on.
      await expect(issueOption(rows.nth(0), "CORE-1")).toBeVisible();
      await expect(rows.nth(1)).toBeFocused();

      // And the reader's next key is still a rove, not a write on the row they left.
      await page.keyboard.press("k");
      await expect(rows.nth(0)).toBeFocused();
      await expect(rows.nth(0).getByRole("button", { name: "Choose issue" })).toContainText(
        "No issue"
      );
    } finally {
      await context.close();
    }
  });

  // A typed message is the reader's work: choosing which issue it belongs to must not throw it
  // away, by either hand. The pick seeds the agent mention an issue-owned comment needs into the
  // draft the reader already has.
  test("a draft survives the issue pick, by keyboard and by pointer", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const toggle = row.getByRole("button", { name: "Choose issue" });

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("please look at the migration");

      // The picker is reached with the pointer while a draft is open - Escape out of a written
      // composer is its own discard prompt, which is not this path - and the pick itself is made
      // from the keyboard the open hands over to.
      await toggle.click();
      await expect(issueSearchIn(row)).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Enter");
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toHaveValue(`${plannerMention} please look at the migration`);
      await expect(field).toBeFocused();

      // And with the pointer all the way: back to no issue, where the mention the channel
      // seeded goes with the channel and the reader's own words stay exactly as typed.
      await toggle.click();
      await issueOption(row, "").click();
      await expect(toggle).toContainText("No issue");
      await expect(field).toHaveValue("please look at the migration");
    } finally {
      await context.close();
    }
  });

  // A file goes to the issue the message was addressed to when the upload started, and its
  // reference names that issue's artifact, even when the reader picks another issue while the
  // file is in the air.
  test("an upload's reference names the issue it went to, not one picked while it was out", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const upload = await holdPosts(page, "**/api/v1/issues/*/artifacts");
      const row = agentRow(page, plannerSession.session_id);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await toggle.click();
      await issueOption(row, "CORE-1").click();
      await expect(field).toHaveValue(`${plannerMention} `);
      await pasteFile(field, "notes.md", "# Notes\n");
      await expect(row.getByRole("button", { name: "Uploading file…" })).toBeDisabled();
      await toggle.click();
      await issueOption(row, "CORE-2").click();
      await expect(toggle).toContainText("CORE-2");
      upload.release();

      await expect(field).toHaveValue(`${plannerMention} dispatch://CORE-1/artifact/notes-md`);
      expect(upload.posts()).toBe(1);
    } finally {
      await context.close();
    }
  });

  // A message on its way is the server's until it answers, and a refusal hands back exactly the
  // draft that was sent. An upload's Retry pressed in the meantime would append its reference to
  // the field for that refusal to drop, leaving the artifact on the issue with nothing pointing
  // at it, so the Retry waits for the answer, as the rest of the composer does.
  test("an upload's Retry waits while a send is out, and its reference joins the draft handed back", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      let uploads = 0;
      // The first upload is refused, so its Retry is on screen when the message goes. A 5xx would
      // be tried again under the upload's key rather than refused.
      await page.route("**/api/v1/issues/*/artifacts", (route) => {
        if (route.request().method() !== "POST") return route.fallback();
        uploads += 1;
        if (uploads > 1) return route.fallback();
        return route.fulfill({
          body: JSON.stringify({ code: "ARTIFACT_INPUT", error: "Dispatch refused the upload" }),
          contentType: "application/json",
          status: 400,
        });
      });
      const refuse = await refusePosts(page, "**/api/v1/issues/*/comments");
      const row = agentRow(page, plannerSession.session_id);
      const field = row.getByRole("textbox", { name: "Comment" });
      const uploadRetry = row
        .getByRole("alert")
        .filter({ hasText: "Dispatch refused the upload" })
        .getByRole("button", { name: "Retry" });

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await row.getByRole("button", { name: "Choose issue" }).click();
      await issueOption(row, "CORE-1").click();
      await expect(field).toHaveValue(`${plannerMention} `);
      await field.press("End");
      await page.keyboard.type("see attached");
      await pasteFile(field, "notes.md", "# Notes\n");
      await expect(uploadRetry).toBeVisible();

      await field.press("Control+Enter");
      await expect(field).toBeDisabled();
      await expect(uploadRetry).toBeDisabled();
      refuse();

      await expect(row.getByText(WRITE_REFUSED_TEXT)).toBeVisible();
      await expect(field).toHaveValue(`${plannerMention} see attached`);
      await uploadRetry.click();
      await expect(field).toHaveValue(
        `${plannerMention} see attached dispatch://CORE-1/artifact/notes-md`
      );
      expect(uploads).toBe(2);
    } finally {
      await context.close();
    }
  });

  // The same slow read, but the reader has gone into the composer rather than to another row:
  // the list's focus belongs to the open, and typing is not where the open left them.
  test("a slow issue list never takes focus out of the message being typed", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      let release: (() => void) | undefined;
      const held = new Promise<void>((resolve) => {
        release = resolve;
      });
      await page.route("**/api/v1/issues?*", async (route) => {
        await held;
        return route.fallback();
      });
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("still typing");

      release?.();
      // The issues have landed, which is when the list decides whether to take the focus; its
      // text box is drawn at the open, before they land.
      await expect(issueOption(row, "CORE-1")).toBeVisible();
      await expect(field).toBeFocused();
      await expect(field).toHaveValue("still typing");
      await expect(row.getByRole("button", { name: "Choose issue" })).toContainText("No issue");
    } finally {
      await context.close();
    }
  });

  // What `i` opens, Escape closes - including when the list never arrives. A list whose issues
  // are still out or failed takes no focus, so it stays on the row, which is exactly where the
  // key has to work.
  test("Escape closes the issue picker from the row when the list fails or is still out", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      let release: (() => void) | undefined;
      const held = new Promise<void>((resolve) => {
        release = resolve;
      });
      await page.route("**/api/v1/issues?*", async (route) => {
        await held;
        return route.fulfill({
          body: JSON.stringify({ code: "INTERNAL", error: "issue store unavailable" }),
          contentType: "application/json",
          status: 500,
        });
      });
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const toggle = row.getByRole("button", { name: "Choose issue" });

      // While the read is still out: the list takes no focus, which stays on the row, and Escape
      // closes what it opened.
      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(row).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(row).toBeFocused();

      // And once it has failed: the list says so, and Escape still leaves.
      release?.();
      await page.keyboard.press("i");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(issueListOf(row).getByText(/Could not load issues/i)).toBeVisible();
      await expect(row).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(row).toBeFocused();

      // Escape on a row with nothing open is the row's own key, as before: it leaves the row.
      await page.keyboard.press("Escape");
      await expect(row).toBeFocused();
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
    } finally {
      await context.close();
    }
  });

  // Enter commits whether or not it changes the issue, and the list closes either way. A
  // commit that left the reader on the document would hand the message they picked the issue
  // for to the page's shortcuts: `j` roves to a row and `x` then ticks its broadcast box.
  test("Enter on the selection already showing still hands the reader to the composer", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = issueSearchIn(row);
      const field = row.getByRole("textbox", { name: "Comment" });
      const broadcast = row.getByRole("checkbox", { name: "Select Planner for broadcast" });

      // "No issue", the selection the picker opens on, confirmed as it stands.
      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("Enter");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(field).toBeFocused();
      await page.keyboard.type("jx");
      await expect(field).toHaveValue("jx");
      await expect(broadcast).not.toBeChecked();

      // The control: a pick that does change the issue lands in the composer too. (The field is
      // emptied first, since Escape over a written composer is its own discard prompt.)
      await field.fill("");
      await page.keyboard.press("Escape");
      await expect(row).toBeFocused();
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Enter");
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toBeFocused();
      await expect(field).toHaveValue(`${plannerMention} `);

      // That issue re-confirmed: nothing changes, and the reader is still handed on.
      await field.fill("");
      await page.keyboard.press("Escape");
      await expect(row).toBeFocused();
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("Enter");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toBeFocused();
      await page.keyboard.type("jx");
      await expect(field).toHaveValue("jx");
      await expect(broadcast).not.toBeChecked();
    } finally {
      await context.close();
    }
  });

  // Escape picks nothing: an arrow moves only the active issue, so the list closes with the
  // message still addressed where it was. A press on an issue is a pick, and it commits at once.
  test("an arrow and Escape pick nothing, and a press on an issue then picks it at once", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = issueSearchIn(row);
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Escape");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(toggle).toContainText("No issue");
      await expect(row).toBeFocused();

      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await issueOption(row, "CORE-2").click();
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(toggle).toContainText("CORE-2");
      await expect(field).toBeFocused();
      await expect(field).toHaveValue(`${plannerMention} `);
    } finally {
      await context.close();
    }
  });

  // A word that matches no issue leaves nothing in the list to pick, so Enter there picks
  // nothing: the list stays open on the word, and the message keeps the address it had.
  test("a word that matches no issue leaves nothing to pick, and Enter picks nothing", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const list = issueListOf(row);
      const picker = issueSearchIn(row);
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.type("zzz");
      await expect(list.getByRole("option")).toHaveCount(0);
      await expect(list.getByText("No issue matches that.")).toBeVisible();
      await page.keyboard.press("Enter");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(picker).toBeFocused();
      await expect(toggle).toContainText("No issue");
      await expect(field).toHaveValue("");
    } finally {
      await context.close();
    }
  });

  // The list opens on the issue the message is addressed to, as a select does, so the arrows
  // move on from that issue rather than from the top of the list.
  test("the list opens on the issue picked, and ArrowUp from it moves to the one above", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = issueSearchIn(row);
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Enter");
      await expect(toggle).toContainText("CORE-2");
      // The field is emptied first, since Escape over a written composer is its own discard
      // prompt.
      await field.fill("");
      await page.keyboard.press("Escape");
      await expect(row).toBeFocused();

      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowUp");
      await page.keyboard.press("Enter");
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // A word typed into the list narrows it, and the arrows then move among the issues left, the
  // first of them active as the word lands.
  test("a typed word narrows the issues, and the arrows move among those left", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = issueSearchIn(row);
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.type("core");
      await expect(issueOption(row, "")).toHaveCount(0);
      await expect(issueListOf(row).getByRole("option")).toHaveCount(2);
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Enter");
      await expect(toggle).toContainText("CORE-2");
      await expect(field).toBeFocused();
      await expect(field).toHaveValue(`${plannerMention} `);
    } finally {
      await context.close();
    }
  });

  // The round trip Rev drove: the mention has to reach the wire, not just the screen, or the
  // agent is never told about the comment that names them.
  test("a comment sent after a round trip through the picker still mentions the agent", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const pick = async (value: string) => {
        await toggle.click();
        await issueOption(row, value).click();
      };

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("hello");

      await pick("CORE-1");
      await pick("");
      await pick("CORE-1");
      // The reader typed ahead of the mention, which is where the caret lands after a pick.
      await field.click();
      await page.keyboard.press("Home");
      await page.keyboard.type("now ");
      await expect(field).toHaveValue(`now ${plannerMention} hello`);

      const posted = page.waitForRequest(
        (request) => request.method() === "POST" && request.url().includes("/comments")
      );
      await page.keyboard.press("Control+Enter");
      const body = (await posted).postDataJSON();
      // A mention is a reference in the text now (R4): the write carries no mentions array,
      // and the party the agent is told about is the link the body holds.
      expect(body.mentions).toBeUndefined();
      expect(body.delivery).toBeTruthy();
      expect(body.body.split(plannerMention)).toHaveLength(2);
    } finally {
      await context.close();
    }
  });

  // A direct message already reaches the session it is addressed to, so the mention the issue
  // channel seeded goes with that channel - and a mention the reader accepted themselves does
  // not: it survives the trip and is on the wire when they come back.
  test("the channel's own mention leaves with it, and the reader's own survives", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const pick = async (value: string) => {
        await toggle.click();
        await issueOption(row, value).click();
      };

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("who owns this");
      await pick("CORE-1");
      await expect(field).toHaveValue(`${plannerMention} who owns this`);

      // The reader mentions the reviewer themselves, inside the issue channel.
      await field.click();
      await page.keyboard.press("End");
      await page.keyboard.type(" @Rev");
      await refPicker(page)
        .getByRole("option", { name: /^Reviewer\b/ })
        .first()
        .click();
      await expect(field).toHaveValue(`${plannerMention} who owns this ${reviewerMention}`);

      // Into the direct channel: the channel's own mention goes, theirs stays.
      await pick("");
      await expect(field).toHaveValue(`who owns this ${reviewerMention}`);
      const sentMessage = page.waitForRequest(
        (request) => request.method() === "POST" && request.url().includes("/messages")
      );
      await field.click();
      await page.keyboard.press("Control+Enter");
      expect((await sentMessage).postDataJSON().body).toBe(`who owns this ${reviewerMention}`);
    } finally {
      await context.close();
    }
  });

  // "Discard draft?" means the draft is gone. On this page `onClose` only moves focus, so a
  // composer that kept the text would hand it to the next channel on the next pick.
  test("Discard clears the draft, and no pick brings it back", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const toggle = row.getByRole("button", { name: "Choose issue" });

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("discard me");
      await page.keyboard.press("Escape");
      await row.getByRole("button", { name: "Discard" }).click();
      await expect(field).toHaveValue("");

      await toggle.click();
      await issueOption(row, "CORE-1").click();
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toHaveValue(`${plannerMention} `);
    } finally {
      await context.close();
    }
  });

  // The composer calls `onClose` after a successful send as well as on the way out, and only the
  // way out is "one level up". Focus on the row after a send would turn the next letters typed
  // into agent shortcuts.
  test("a sent message leaves focus in the composer, not on the row", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      await page.keyboard.press("j");
      await expect(row).toBeFocused();
      await page.keyboard.press("Enter");
      const composer = row.getByRole("textbox", { name: "Comment" });
      await expect(composer).toBeFocused();

      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");
      // The composer clears only once the server has taken the message.
      await expect(composer).toHaveValue("");
      await expect(composer).toBeFocused();
      await page.keyboard.type("j");
      await expect(composer).toHaveValue("j");
      await expect(row).not.toBeFocused();
    } finally {
      await context.close();
    }
  });

  // The disable takes focus to the document; only that focus is the composer's to take back. A
  // reader who moved on during the round trip keeps where they went.
  test("a send in flight does not steal back focus the reader has moved", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const rows = shownAgentRows(page);
      const planner = rows.nth(0);
      const reviewer = rows.nth(1);

      // Hold the send open so the reader has a window to click in, as a slow server would.
      const send = await holdPosts(page, "**/api/v1/agents/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      const composer = planner.getByRole("textbox", { name: "Comment" });
      await expect(composer).toBeFocused();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");

      // Mid-send the reader ticks the other agent's box; that focus is theirs to keep.
      const reviewerBox = reviewer.getByRole("checkbox", { name: "Select Reviewer for broadcast" });
      await reviewerBox.focus();
      await expect(reviewerBox).toBeFocused();
      send.release();
      await expect(composer).toHaveValue("");
      await expect(reviewerBox).toBeFocused();
      await expect(composer).not.toBeFocused();
    } finally {
      await context.close();
    }
  });

  // `i` clicks a button that only renders while the row is not replying to a message, so it is
  // advertised only when that button is reachable - the rule this slice already applied to the
  // Inbox's own snooze.
  test("i is listed as unavailable while the row is answering a message", async ({ browser }) => {
    const issueKey = await seedAgents();
    const root = await createMessage(issueKey, {
      body: "Can this ship?",
      delivery: "btw",
      target: `session:${plannerSession.session_id}`,
    });
    expect(root.id).toBeTruthy();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(row.getByRole("textbox", { name: "Comment" })).toBeFocused();
      await expect(row.getByRole("button", { name: "Choose issue" })).toBeVisible();

      await row.getByRole("button", { name: "Reply" }).first().click();
      await expect(row.getByRole("button", { name: "Choose issue" })).toHaveCount(0);
      await row.focus();

      await page.keyboard.press("?");
      const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
      await expect(
        help.getByRole("listitem").filter({ hasText: "Pick an issue for the message" })
      ).toHaveAttribute("data-enabled", "false");
    } finally {
      await context.close();
    }
  });
});
