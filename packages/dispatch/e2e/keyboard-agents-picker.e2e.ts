import {
  agentRow,
  issueListOf,
  issueOption,
  issueSearchIn,
  openAgents,
  plannerMention,
  plannerSession,
  seedAgents,
  shownAgentRows,
} from "./agents";
import { createAgentMessage, createMessage, listComments, patchIssue } from "./api";
import { openFromSidebar } from "./editor";
import { keyedWrite } from "./keyed-writes";
import { holdPosts, MESSAGE_REFUSED, refusePosts } from "./sends";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

// The composer's Issue list is the shared single-pick list (`MultiSelect`, `mode: "single"`): a
// text box that narrows the open issues, arrows that move the active issue, Enter that picks
// it, and Tab, Shift+Tab, Escape and a press outside that pick nothing. The five rows below that
// drive that rule from the keyboard - the arrows and Enter, an arrow left by Tab and by
// Shift+Tab, an arrow from No issue left by Tab, and a typed word - carry `@webkit @firefox`, the
// tags those two projects select by (`playwright.config.ts`), and drive keys through
// `page.keyboard`, which every engine has: each engine delivers keys to a text box, and takes the
// focus out of one on Tab, its own way. One row more carries `@webkit` alone, for a reason of its
// own the comment above it gives: the send held across two in-app navigations, whose every
// measured failure was in WebKit. Every other row of this file is untagged: what the list and
// Reply do with a message on its way to the server, what a press outside the list does, and what
// a pick dispatched from a script does, are the same in every engine, and Chromium asserts them.
// The list is portalled out of the row, so every row reaches it through the panel the row's own
// trigger names (`issueListOf`, `issueOption`).

/** What a row's composer says once Dispatch refuses its message. A server that is down is tried
 *  again under the send's key, so the rows that need a refusal refuse with Dispatch's verdict. */
const messageRefused = `Couldn't send — ${MESSAGE_REFUSED.error}`;

/** The send the rows below make, and the path of the one `POST` it produces. A comment and a
 *  direct message share this shape, so the row that calls this names the path it expects. */
async function sendAndCapturePath(page: Page, field: Locator): Promise<string> {
  const sent = page.waitForRequest(
    (request) =>
      request.method() === "POST" && /\/(comments|messages)$/.test(new URL(request.url()).pathname)
  );
  await field.fill("Status please");
  await field.press("Control+Enter");
  return new URL((await sent).url()).pathname;
}

/** The id of the option of a row's open Issue list that picks `key`, which the list's text box
 *  names in `aria-activedescendant` while that option is the active one. */
async function optionIdOf(row: Locator, key: string): Promise<string> {
  // `getAttribute` waits for the option to attach, and `MultiSelect` writes its id on that same
  // element in the render that draws it, so the first answer is the option's id.
  const id = await issueOption(row, key).getAttribute("id");
  if (id === null) throw new Error(`the Issue list's option for "${key}" has no id`);
  return id;
}

test.describe("agents page", () => {
  // The arrows move the active issue and Enter picks it, so a reader can pass the first issue to
  // reach the second - and the Enter that picks is the list's, not a newline at the top of the
  // message. Escape after an arrow picks nothing.
  // @webkit @firefox: the list's keys, as each engine delivers them to its text box.
  test("the arrows move the active issue and Enter picks it, and Escape after an arrow picks nothing @webkit @firefox", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      const picker = issueSearchIn(row);
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("ArrowDown");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(picker).toBeFocused();

      await page.keyboard.press("Enter");
      await expect(toggle).toContainText("CORE-2");
      await expect(field).toBeFocused();
      await expect(field).toHaveValue(`${plannerMention} `);

      // Escape after an arrow picks nothing: the issue picked stays. (The field is emptied first,
      // since Escape over a written composer is its own discard prompt.)
      await field.fill("");
      await page.keyboard.press("Escape");
      await expect(row).toBeFocused();
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowUp");
      await page.keyboard.press("Escape");
      await expect(toggle).toHaveAttribute("aria-expanded", "false");
      await expect(toggle).toContainText("CORE-2");
      await expect(row).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // An arrow is not a pick, so a reader who moves the active issue and then leaves the list any
  // other way has picked nothing: the list closes, and the toggle and the send name the issue the
  // message was addressed to.
  for (const [way, tags, leave] of [
    ["Tab", " @webkit @firefox", (page: Page) => page.keyboard.press("Tab")],
    ["Shift+Tab", " @webkit @firefox", (page: Page) => page.keyboard.press("Shift+Tab")],
    ["a click into the message", "", (_page: Page, field: Locator) => field.click()],
  ] as const) {
    // @webkit @firefox for the two keys: the list's Tab and Shift+Tab, as each engine delivers
    // them to its text box. A click into the message is the same press in every engine.
    test(`an arrow left by ${way} picks nothing, so the toggle and the send name one issue${tags}`, async ({
      browser,
    }) => {
      const issueKey = await seedAgents();
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
        await page.keyboard.press("Enter");
        await expect(toggle).toContainText(issueKey);
        await field.fill("");
        await page.keyboard.press("Escape");
        await expect(row).toBeFocused();

        await page.keyboard.press("i");
        await expect(picker).toBeFocused();
        const picked = await optionIdOf(row, issueKey);
        await expect(picker).toHaveAttribute("aria-activedescendant", picked);
        await page.keyboard.press("ArrowDown");
        await expect(picker).not.toHaveAttribute("aria-activedescendant", picked);
        await leave(page, field);
        await expect(picker).toHaveCount(0);
        await expect(toggle).toHaveAttribute("aria-expanded", "false");
        await expect(toggle).toContainText(issueKey);

        expect(await sendAndCapturePath(page, field)).toBe(`/api/v1/issues/${issueKey}/comments`);
      } finally {
        await context.close();
      }
    });
  }

  // From `No issue` the message is a direct one, so an arrow left by Tab must not address it to
  // an issue.
  // @webkit @firefox: the list's arrow and Tab, as each engine delivers them to its text box.
  test("an arrow from No issue left by Tab picks nothing, and the message goes to the agent directly @webkit @firefox", async ({
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
      await expect(picker).toHaveAttribute(
        "aria-activedescendant",
        await optionIdOf(row, "CORE-1")
      );
      await page.keyboard.press("Tab");
      await expect(picker).toHaveCount(0);
      await expect(toggle).toContainText("No issue");

      expect(await sendAndCapturePath(page, field)).toBe(
        `/api/v1/agents/${plannerSession.session_id}/messages`
      );
    } finally {
      await context.close();
    }
  });

  // A word typed into the list leaves only the issues it matches, and Enter picks the one left.
  // @webkit @firefox: the typed keys, as each engine delivers them to the list's text box.
  test("a word typed into the list leaves the issues it matches, and Enter picks the one left @webkit @firefox", async ({
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
      await page.keyboard.type("rollout");
      await expect(issueListOf(row).getByRole("option")).toHaveCount(1);
      await expect(issueOption(row, "CORE-2")).toBeVisible();
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(picker).toBeFocused();

      await page.keyboard.press("Enter");
      await expect(toggle).toContainText("CORE-2");
      await expect(field).toBeFocused();
      await expect(field).toHaveValue(`${plannerMention} `);
    } finally {
      await context.close();
    }
  });

  // A message on its way to the server is addressed already, so its issue cannot change under it,
  // and the text the server is taking must never come back to the composer, enabled - one
  // Ctrl+Enter from sending it twice.
  // @pr: A write on the wire: the picker is held while a send is out, and the text the server is taking never comes back to the composer.
  test("a send in flight holds the picker, and the sent text never comes back @pr", async ({
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
      const send = await holdPosts(page, "**/api/v1/agents/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await toggle.click();
      await expect(picker).toBeFocused();
      // The click into the message is a press outside the list, which closes it; what holds the
      // picker mid-send is its trigger, the toggle.
      await field.click();
      await expect(picker).toHaveCount(0);
      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");

      await expect(field).toBeDisabled();
      await expect(toggle).toHaveAttribute("aria-disabled", "true");
      // `?` says so as well: from the row, `i` is greyed while the picker it would open is held.
      await row.focus();
      await page.keyboard.press("?");
      const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
      await expect(
        help.getByRole("listitem").filter({ hasText: "Pick an issue for the message" })
      ).toHaveAttribute("data-enabled", "false");
      await page.keyboard.press("Escape");
      await expect(help).toHaveCount(0);
      send.release();
      await expect(field).toBeEnabled();
      await expect(field).toHaveValue("");
      await expect(toggle).toBeEnabled();

      // The pick the reader could not make mid-send takes nothing from the message already sent.
      await toggle.click();
      await issueOption(row, "CORE-1").click();
      await expect(toggle).toContainText("CORE-1");
      await expect(field).toHaveValue(`${plannerMention} `);
      expect(send.posts()).toBe(1);
    } finally {
      await context.close();
    }
  });

  // A reply to a message on an issue is written on that issue, and the send ends the reply, which
  // takes the composer back to the direct channel: it must come back empty.
  test("a reply sent on an issue leaves the direct composer empty", async ({ browser }) => {
    const issueKey = await seedAgents();
    await createMessage(issueKey, {
      body: "Can this ship?",
      delivery: "btw",
      target: `session:${plannerSession.session_id}`,
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const cancelReply = row.getByRole("button", { name: "Cancel reply" });

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await row.getByRole("button", { name: "Reply" }).first().click();
      await expect(cancelReply).toBeVisible();
      await field.fill("Once the build is green");
      const sent = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" &&
          new URL(response.url()).pathname === `/api/v1/issues/${issueKey}/messages`
      );
      await field.press("Control+Enter");
      expect((await sent).ok()).toBe(true);

      await expect(cancelReply).toHaveCount(0);
      await expect(row.getByRole("button", { name: "Choose issue" })).toContainText("No issue");
      await expect(field).toHaveValue("");
      await expect(field).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // Ending the reply while it is in the air would move the message to another channel under the
  // send, so Cancel reply holds until the server answers, as Reply and the picker do. The send
  // then ends the reply itself, and the text it took does not come back.
  test("Cancel reply holds while a reply is out, and the sent text never comes back", async ({
    browser,
  }) => {
    const issueKey = await seedAgents();
    await createMessage(issueKey, {
      body: "Can this ship?",
      delivery: "btw",
      target: `session:${plannerSession.session_id}`,
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const send = await holdPosts(page, "**/api/v1/issues/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await row.getByRole("button", { name: "Reply" }).first().click();
      await field.fill("Once the build is green");
      await field.press("Control+Enter");
      await expect(field).toBeDisabled();
      await expect(row.getByRole("button", { name: "Cancel reply" })).toBeDisabled();
      send.release();

      await expect(row.getByRole("button", { name: "Cancel reply" })).toHaveCount(0);
      await expect(field).toBeEnabled();
      await expect(field).toHaveValue("");
      await expect(row.getByRole("button", { name: "Choose issue" })).toContainText("No issue");
      await expect(field).toBeFocused();
      expect(send.posts()).toBe(1);
    } finally {
      await context.close();
    }
  });

  // The reply a refused send was for stays until the reader ends it. Ended then, the message goes
  // back to the issue picked and the draft takes that issue's mention, so Retry reaches the agent
  // there - a direct reply's refusal retried as an issue comment that still tells the agent.
  test("a reply refused and then cancelled retries on the issue, mentioning the agent", async ({
    browser,
  }) => {
    const issueKey = await seedAgents();
    await createAgentMessage(plannerSession.session_id, {
      body: "Are you free?",
      delivery: "btw",
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = agentRow(page, plannerSession.session_id);
      const field = row.getByRole("textbox", { name: "Comment" });
      const cancelReply = row.getByRole("button", { name: "Cancel reply" });
      const picker = issueSearchIn(row);
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages", MESSAGE_REFUSED);

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      // The list takes focus once its issues land; the arrows are for it, not the row.
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Enter");
      await expect(field).toHaveValue(`${plannerMention} `);
      // A reply to a direct exchange is a direct message, whatever issue is picked.
      await row.getByRole("button", { name: "Reply" }).first().click();
      await field.fill("Yes");
      await field.press("Control+Enter");
      await expect(field).toBeDisabled();
      await expect(cancelReply).toBeDisabled();
      refuse();

      await expect(row.getByText(messageRefused)).toBeVisible();
      await expect(field).toHaveValue("Yes");
      await cancelReply.click();
      await expect(row.getByRole("button", { name: "Choose issue" })).toContainText(issueKey);
      await expect(field).toHaveValue(`${plannerMention} Yes`);
      const retried = page.waitForRequest(
        (request) =>
          request.method() === "POST" &&
          new URL(request.url()).pathname === `/api/v1/issues/${issueKey}/comments`
      );
      await row.getByRole("button", { name: "Retry" }).click();
      // Planner takes Aside and BTW but not Send, so a plain message to it is an Aside. The
      // party it names is in its own text, so no mentions array goes with it.
      expect((await retried).postDataJSON()).toEqual({
        body: `${plannerMention} Yes`,
        delivery: "aside",
      });
    } finally {
      await context.close();
    }
  });

  // A message on its way is addressed, so Reply holds until the server answers, as the picker
  // does. A refusal hands back the draft that was sent, and Retry sends exactly that again.
  test("Reply holds while a send is out, and a refusal's Retry posts exactly what was refused", async ({
    browser,
  }) => {
    const issueKey = await seedAgents();
    await createMessage(issueKey, {
      body: "Can this ship?",
      delivery: "btw",
      target: `session:${plannerSession.session_id}`,
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const reply = row.getByRole("button", { name: "Reply" }).first();
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages", MESSAGE_REFUSED);
      const sent = page.waitForRequest(
        (request) =>
          request.method() === "POST" &&
          new URL(request.url()).pathname === `/api/v1/agents/${plannerSession.session_id}/messages`
      );

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");
      await expect(field).toBeDisabled();
      await expect(reply).toHaveAttribute("aria-disabled", "true");
      refuse();

      await expect(row.getByText(messageRefused)).toBeVisible();
      await expect(field).toBeEnabled();
      await expect(field).toHaveValue("Status please");
      await expect(reply).toBeEnabled();
      const retried = page.waitForRequest(
        (request) =>
          request.method() === "POST" &&
          new URL(request.url()).pathname === `/api/v1/agents/${plannerSession.session_id}/messages`
      );
      await row.getByRole("button", { name: "Retry" }).click();
      // The same message under a new key: Dispatch keeps the answer it gave a key, so the refused
      // key would only be answered the refusal again.
      const first = keyedWrite(await sent);
      const again = keyedWrite(await retried);
      expect(first).toEqual({
        input: { body: "Status please", delivery: "aside" },
        key: expect.any(String),
      });
      expect(again).toEqual({ input: first.input, key: expect.any(String) });
      expect(again.key).not.toBe(first.key);
    } finally {
      await context.close();
    }
  });

  // A direct send owns its address in the same task that begins it. A Reply click before React
  // re-renders must not move that in-flight request onto this issue's message route.
  test("a same-task Reply cannot re-address a direct send", async ({ browser }) => {
    const issueKey = await seedAgents();
    await createMessage(issueKey, {
      body: "Can this ship?",
      delivery: "btw",
      target: `session:${plannerSession.session_id}`,
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages", MESSAGE_REFUSED);
      const sent = page.waitForRequest(
        (request) =>
          request.method() === "POST" &&
          /\/api\/v1\/(agents\/[^/]+|issues\/[^/]+)\/messages$/.test(
            new URL(request.url()).pathname
          )
      );

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await field.fill("Status please");
      await row.evaluate((node) => {
        const send = node.querySelector<HTMLButtonElement>('button[type="submit"]');
        const reply = node.querySelector<HTMLButtonElement>('button[aria-label="Reply"]');
        if (send === null || reply === null) throw new Error("expected send and Reply controls");
        send.click();
        reply.click();
      });

      expect(new URL((await sent).url()).pathname).toBe(
        `/api/v1/agents/${plannerSession.session_id}/messages`
      );
      refuse();
      await expect(row.getByText(messageRefused)).toBeVisible();
      await expect(field).toHaveValue("Status please");
      await expect(row.getByRole("button", { name: "Cancel reply" })).toHaveCount(0);
    } finally {
      await context.close();
    }
  });

  // An already-open list can pick an issue in the same task as Send. Its pick must not turn the
  // direct send into an issue message before the request takes its owner.
  test("a same-task issue pick cannot re-address a direct send", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = issueSearchIn(row);
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages", MESSAGE_REFUSED);
      const sent = page.waitForRequest(
        (request) =>
          request.method() === "POST" &&
          /\/api\/v1\/(agents\/[^/]+|issues\/[^/]+)\/messages$/.test(
            new URL(request.url()).pathname
          )
      );

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await toggle.click();
      await expect(picker).toBeVisible();
      await field.fill("Status please");
      const option = await issueOption(row, "CORE-1").elementHandle();
      await row.evaluate((node, pick) => {
        const send = node.querySelector<HTMLButtonElement>('button[type="submit"]');
        if (send === null || !(pick instanceof HTMLElement))
          throw new Error("expected send and the issue's option");
        send.click();
        pick.click();
      }, option);

      expect(new URL((await sent).url()).pathname).toBe(
        `/api/v1/agents/${plannerSession.session_id}/messages`
      );
      refuse();
      await expect(row.getByText(messageRefused)).toBeVisible();
      await expect(field).toHaveValue("Status please");
      await expect(toggle).toContainText("No issue");
    } finally {
      await context.close();
    }
  });

  // An upload carries its target from the paste that starts it, as a send carries its request: a
  // pick in the paste's own task, before React re-renders, moves where the next message goes but
  // not where this file goes, and the reference the upload adds names the issue that holds it.
  test("a same-task issue pick cannot move an upload", async ({ browser }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = agentRow(page, plannerSession.session_id);
      const field = row.getByRole("textbox", { name: "Comment" });
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const uploaded = page.waitForRequest(
        (request) =>
          request.method() === "POST" &&
          /\/api\/v1\/issues\/[^/]+\/artifacts$/.test(new URL(request.url()).pathname)
      );

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await toggle.click();
      await issueOption(row, "CORE-1").click();
      await expect(field).toHaveValue(`${plannerMention} `);
      await toggle.click();
      const option = await issueOption(row, "CORE-2").elementHandle();
      await row.evaluate((node, pick) => {
        const textarea = node.querySelector<HTMLTextAreaElement>('textarea[aria-label="Comment"]');
        if (textarea === null || !(pick instanceof HTMLElement))
          throw new Error("expected the composer and the issue's option");
        const data = new DataTransfer();
        data.items.add(new File(["# Notes\n"], "notes.md", { type: "text/markdown" }));
        const paste = new Event("paste", { bubbles: true, cancelable: true });
        Object.defineProperty(paste, "clipboardData", { value: data });
        textarea.dispatchEvent(paste);
        pick.click();
      }, option);

      expect(new URL((await uploaded).url()).pathname).toBe("/api/v1/issues/CORE-1/artifacts");
      await expect(field).toHaveValue(`${plannerMention} dispatch://CORE-1/artifact/notes-md`);
      await expect(toggle).toContainText("CORE-2");
    } finally {
      await context.close();
    }
  });

  // Cancel reply is the same address mutation in reverse. The send retains its direct reply
  // target until its refusal arrives, even when the click shares Send's task.
  test("a same-task Cancel reply keeps the direct reply it sent", async ({ browser }) => {
    await seedAgents();
    await createAgentMessage(plannerSession.session_id, {
      body: "Are you free?",
      delivery: "btw",
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages", MESSAGE_REFUSED);
      const sent = page.waitForRequest(
        (request) =>
          request.method() === "POST" &&
          new URL(request.url()).pathname === `/api/v1/agents/${plannerSession.session_id}/messages`
      );

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await row.getByRole("button", { name: "Reply" }).first().click();
      await field.fill("Yes");
      await row.evaluate((node) => {
        const send = node.querySelector<HTMLButtonElement>('button[type="submit"]');
        const cancel = node.querySelector<HTMLButtonElement>('button[aria-label="Cancel reply"]');
        if (send === null || cancel === null)
          throw new Error("expected send and Cancel reply controls");
        send.click();
        cancel.click();
      });

      expect(new URL((await sent).url()).pathname).toBe(
        `/api/v1/agents/${plannerSession.session_id}/messages`
      );
      refuse();
      await expect(row.getByText(messageRefused)).toBeVisible();
      await expect(field).toHaveValue("Yes");
      await expect(row.getByRole("button", { name: "Cancel reply" })).toBeVisible();
    } finally {
      await context.close();
    }
  });

  // A refusal is about the draft it turned down. Retry sends the draft as it stands, so it is
  // offered only when Send would be; Discard drops the draft, and the notice goes with it.
  test("Retry asks what Send asks, and Discard takes a refusal's notice with the draft", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const notice = row.getByText(messageRefused);
      const retry = row.getByRole("button", { name: "Retry" });
      let posts = 0;
      page.on("request", (request) => {
        if (request.method() === "POST" && new URL(request.url()).pathname.endsWith("/messages")) {
          posts += 1;
        }
      });
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages", MESSAGE_REFUSED);

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");
      refuse();
      await expect(notice).toBeVisible();
      await expect(retry).toBeVisible();

      // Nothing Send would send, so nothing Retry may: the button goes, the notice stays.
      await field.fill("");
      await expect(retry).toHaveCount(0);
      await expect(notice).toBeVisible();

      await field.fill("Status please");
      await field.press("Escape");
      await row.getByRole("button", { name: "Discard" }).click();
      await expect(notice).toHaveCount(0);
      await expect(retry).toHaveCount(0);
      await expect(field).toHaveValue("");
      expect(posts).toBe(1);
    } finally {
      await context.close();
    }
  });

  // A message on its way is the server's until it answers. A `Discard draft?` the reader raised
  // before sending goes with the send, and the field holds, so nothing they discard or type in the
  // meantime can be overwritten by the outcome: the refusal hands back the draft that was sent,
  // with its notice and Retry.
  test("a send takes the Discard prompt with it, and a refusal hands back the draft with its notice", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = shownAgentRows(page).nth(0);
      const field = row.getByRole("textbox", { name: "Comment" });
      const discard = row.getByRole("button", { name: "Discard" });
      const refuse = await refusePosts(page, "**/api/v1/agents/*/messages", MESSAGE_REFUSED);

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Escape");
      await expect(discard).toBeVisible();
      await expect(field).toBeFocused();
      await page.keyboard.press("Control+Enter");
      await expect(field).toBeDisabled();
      await expect(discard).toHaveCount(0);
      refuse();

      await expect(row.getByText(messageRefused)).toBeVisible();
      await expect(field).toBeEnabled();
      await expect(field).toHaveValue("Status please");
      await expect(row.getByRole("button", { name: "Retry" })).toBeVisible();
      await expect(discard).toHaveCount(0);
    } finally {
      await context.close();
    }
  });

  // The list is every open issue, so an issue closed after it was picked drops out of it on the
  // next read. The Issue list keeps offering the committed issue, marked closed and picked - as
  // the issue header's Status pick keeps a closed issue's own status among its options - so
  // what the list shows, what the toggle names and where the message goes stay one issue until
  // the reader picks another; a closed issue takes comments, so the message posts there.
  test("an issue closed after the pick stays named in the list and the toggle, marked closed, and takes the message", async ({
    browser,
  }) => {
    const issueKey = await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      // By id: closing the issue closes the Planner's ask on it, which moves its row below the
      // Reviewer's.
      const row = agentRow(page, plannerSession.session_id);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const list = issueListOf(row);
      const picker = issueSearchIn(row);
      const field = row.getByRole("textbox", { name: "Comment" });

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Enter");
      await expect(toggle).toContainText(issueKey);

      await patchIssue(issueKey, { status: "done" });
      await toggle.click();
      await expect(picker).toBeVisible();
      // The reader comes back to the tab once the list has gone stale (30 s), which refetches it -
      // the page's own way to learn of the close, since no event names this list.
      await page.evaluate(() => {
        const now = Date.now();
        Date.now = () => now + 31_000;
        window.dispatchEvent(new Event("visibilitychange"));
        window.dispatchEvent(new Event("focus"));
      });
      await expect(list.getByRole("option", { name: /Rollout plan/ })).toHaveCount(1);
      await expect(list.getByRole("option", { name: /Keyboard issue/ })).toHaveCount(0);
      await expect(list.getByRole("option", { selected: true })).toHaveAccessibleName(
        `${issueKey} (closed)`
      );
      await expect(toggle).toContainText(`${issueKey} (closed)`);

      const posted = page.waitForResponse(
        (response) =>
          response.request().method() === "POST" &&
          response.url().endsWith(`/api/v1/issues/${issueKey}/comments`)
      );
      expect(await sendAndCapturePath(page, field)).toBe(`/api/v1/issues/${issueKey}/comments`);
      expect((await posted).status()).toBe(201);
      await expect(field).not.toHaveValue(/Status please/);
      expect(
        (await listComments(issueKey)).some((comment) => comment.body.includes("Status please"))
      ).toBe(true);

      // Picking another issue ends it: the closed one is no longer offered at all.
      await issueOption(row, "CORE-2").click();
      await expect(toggle).toContainText("CORE-2");
      await toggle.click();
      await expect(issueOption(row, issueKey)).toHaveCount(0);
    } finally {
      await context.close();
    }
  });

  // An issue comment reaches this agent only by mentioning it, so the reset a send makes seeds the
  // mention again, as a mount does - the next message is as addressed as the first. A direct
  // message already reaches its session, and its channel seeds nothing.
  test("every message sent on an issue mentions the agent, and a direct one carries none", async ({
    browser,
  }) => {
    const issueKey = await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = agentRow(page, plannerSession.session_id);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = issueSearchIn(row);
      const field = row.getByRole("textbox", { name: "Comment" });
      const sends: { input: unknown; key: string | undefined; ok: boolean; path: string }[] = [];
      page.on("response", (response) => {
        const request = response.request();
        const path = new URL(request.url()).pathname;
        if (request.method() !== "POST" || !/\/(comments|messages)$/.test(path)) return;
        sends.push({ ...keyedWrite(request), ok: response.ok(), path });
      });
      // Typed, not filled: the composer tracks its mention and delivery as the keys arrive, and
      // a `fill` of the same text sends neither (measured).
      const send = async (text: string) => {
        const before = sends.length;
        await field.click();
        await page.keyboard.press("End");
        await page.keyboard.type(text);
        await page.keyboard.press("Control+Enter");
        await expect
          .poll(() => sends.length, { intervals: [50, 50, 100, 200, 500] })
          .toBe(before + 1);
      };

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(picker).toBeFocused();
      await page.keyboard.press("ArrowDown");
      await page.keyboard.press("Enter");
      await expect(toggle).toContainText(issueKey);
      await expect(field).toHaveValue(`${plannerMention} `);

      await send("first");
      await expect(field).toHaveValue(`${plannerMention} `);
      await send("second");
      await expect(field).toHaveValue(`${plannerMention} `);

      await toggle.click();
      await issueOption(row, "").click();
      await expect(field).toHaveValue("");
      await send("third");
      await expect(field).toHaveValue("");

      // Every send is keyed, each under a key of its own.
      const key = expect.any(String);
      expect(sends).toEqual([
        {
          input: { body: `${plannerMention} first`, delivery: "aside" },
          key,
          ok: true,
          path: `/api/v1/issues/${issueKey}/comments`,
        },
        {
          input: { body: `${plannerMention} second`, delivery: "aside" },
          key,
          ok: true,
          path: `/api/v1/issues/${issueKey}/comments`,
        },
        {
          input: { body: "third", delivery: "aside" },
          key,
          ok: true,
          path: `/api/v1/agents/${plannerSession.session_id}/messages`,
        },
      ]);
      expect(new Set(sends.map((send) => send.key)).size).toBe(3);
    } finally {
      await context.close();
    }
  });

  // `i` opens the Issue list; it never shuts one. A list opened before its issues came takes no
  // focus and stays open while the reader is on the row, so `i` pressed there again goes into its
  // text box rather than clicking the toggle closed, and the issues landing leave the reader there.
  test("i on a row whose Issue list is open goes into it rather than shutting it", async ({
    browser,
  }) => {
    await seedAgents();
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      const held = Promise.withResolvers<void>();
      await page.route("**/api/v1/issues?*", async (route) => {
        await held.promise;
        return route.fallback();
      });
      await openAgents(page);
      const row = agentRow(page, plannerSession.session_id);
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const picker = issueSearchIn(row);

      await page.keyboard.press("j");
      await page.keyboard.press("i");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(row).toBeFocused();
      await page.keyboard.press("i");
      await expect(toggle).toHaveAttribute("aria-expanded", "true");
      await expect(picker).toBeFocused();

      held.resolve();
      await expect(issueOption(row, "CORE-1")).toBeVisible();
      await expect(picker).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // A message on its way holds the picker and Reply of the row that sent it, and its draft, until
  // the server answers. A reader who leaves the page mid-send and comes back finds the row holding
  // the same send - the draft, the picker and Reply held - and its answer lands there.
  // @webkit under the second of the two reasons for an engine tag (`e2e/playwright.config.ts`,
  // the comment above `tagged`): not a WebKit behaviour of its own - a send held open across two
  // in-app navigations is the same in every engine - but the engine this row's own failures were
  // in, every one of six, one run on each of six branches, where the `g i` chord's second key
  // landed after the keymap had dropped the chord and the reader never left the page; the
  // sidebar's links carry no such deadline (`openFromSidebar`, `e2e/editor.ts`). The tag is what
  // keeps the row there now that the projects select rows by tag rather than by file
  // (`webkitTagged`); without it the row would run only where its defect was never seen.
  test("a row back after the reader left mid-send holds the send it left, which then lands there @webkit", async ({
    browser,
  }) => {
    const issueKey = await seedAgents();
    await createMessage(issueKey, {
      body: "Can this ship?",
      delivery: "btw",
      target: `session:${plannerSession.session_id}`,
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await openAgents(page);
      const row = agentRow(page, plannerSession.session_id);
      const field = row.getByRole("textbox", { name: "Comment" });
      const toggle = row.getByRole("button", { name: "Choose issue" });
      const reply = row.getByRole("button", { name: "Reply" }).first();
      const send = await holdPosts(page, "**/api/v1/agents/*/messages");

      await page.keyboard.press("j");
      await page.keyboard.press("Enter");
      await expect(field).toBeFocused();
      await page.keyboard.type("Status please");
      await page.keyboard.press("Control+Enter");
      await expect(field).toBeDisabled();
      await expect(toggle).toHaveAttribute("aria-disabled", "true");
      await expect(reply).toHaveAttribute("aria-disabled", "true");

      // Away to the Inbox and back without a reload, so the app - and the send - stay loaded,
      // by the sidebar's own links, which openFromSidebar explains.
      await openFromSidebar(page, "Inbox");
      await expect(row).toHaveCount(0);
      await openFromSidebar(page, "Agents");
      await row.getByRole("button", { exact: true, name: "Planner" }).click();

      await expect(field).toBeDisabled();
      await expect(field).toHaveValue("Status please");
      await expect(toggle).toHaveAttribute("aria-disabled", "true");
      await expect(reply).toHaveAttribute("aria-disabled", "true");
      send.release();
      await expect(field).toHaveValue("");
      await expect(field).toBeEnabled();
      await expect(toggle).toBeEnabled();
      await expect(reply).toBeEnabled();
      expect(send.posts()).toBe(1);
    } finally {
      await context.close();
    }
  });
});
