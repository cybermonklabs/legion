import {
  type DispatchWriting,
  type DispatchWritingBox,
  dispatchWritingBox,
} from "@legion/contracts";

import { REF_PICKER_WORDS } from "../web/src/features/refs/ref-picker";
import { agentRow, getSentMessages } from "./agents";
import {
  answerAsk,
  createIssueArtifact,
  getArtifactText,
  publishAgentStreamFrame,
  requestApproval,
} from "./api";
import {
  barAction,
  composer,
  documentEditor,
  marginCard,
  openSpec,
  selectEditorText,
} from "./editor";
import { askCard, inboxPane, selectRow } from "./inbox-pane";
import { replyRowId } from "./inbox-threads";
import { commentField, pickerRows, pickReference, refPicker } from "./ref-picker";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";
import {
  ANCHOR,
  type BoxWorld,
  coveredBoxes,
  DOOR_ROWS,
  expectDeliveryClaim,
  expectHint,
  expectIssueRows,
  keyboardOnly,
  NO_MATCH,
  OFF_ISSUE_PERSON,
  QUERY,
  rowsOfBox,
  SESSION,
  seedArchitecture,
  seedWritingBoxes,
  walkBox,
} from "./writing-boxes";

/**
 * INFRA-1702's `c-field-everywhere`: the same picker in every box a person writes in.
 *
 * One `test()` per covered box of `DISPATCH_WRITING_BOXES`, generated from that list, so a box
 * the list gains with no way to drive it fails a row of its own and a driver for a box the list
 * no longer holds fails every row. In each box the three doors open over the same rows in each
 * door's own order, the words of a list with no rows are the same, Enter takes the highlighted
 * row and the pick lands at the caret with the words after it untouched, and the line under the
 * box is the hint its row of the list holds - read from the list rather than written out again.
 * Which people a box lists is the one thing its own row decides (`issueRows`), and each box is
 * held to its row's answer.
 *
 * `e2e/writing-boxes.ts` holds the world and the checks; this file holds how each box is
 * reached. Two boxes draw the picker in an editor of their own rather than in the shared text
 * field, and carry what is theirs alone: the document body, where a pick goes through the
 * editor's own transaction into the document's stored text and reaches another reader of it,
 * and the agent live conversation, where `/` still opens the session's commands and Enter takes
 * a row rather than sending the message.
 */

test.setTimeout(120_000);

/** The box's hint, the people it lists and what it says it does with a picked party, which are
 *  what its own row of the list decides. */
async function expectRow(
  page: Page,
  field: Locator,
  box: DispatchWritingBox,
  write: DispatchWriting
): Promise<void> {
  await expectHint(page, field, box, write);
  await expectIssueRows(field, dispatchWritingBox(box, write).issueRows);
  await expectDeliveryClaim(field, box, write);
}

/** How one box is reached and driven. `seed` runs before the browser opens. */
interface Driver {
  readonly seed?: (world: BoxWorld) => Promise<void>;
  readonly drive: (page: Page, world: BoxWorld) => Promise<void>;
}

/** Opens the margin's composer of `kind` over the spec's anchor paragraph. */
async function marginComposer(page: Page, kind: "Comment" | "Suggest" | "Ask"): Promise<Locator> {
  await selectEditorText(page, ANCHOR);
  await barAction(page, kind);
  return composer(page);
}

/** The margin's own thread, expanded beside the document. */
async function openMarginCard(page: Page, id: string): Promise<Locator> {
  const card = marginCard(page, id);
  await card.getByRole("button", { expanded: false }).first().click();
  await expect(card).toHaveAttribute("aria-expanded", "true");
  return card;
}

const DRIVERS: Partial<Record<DispatchWritingBox, Driver>> = {
  "agent-live-composer": {
    drive: async (page, world) => {
      const sent: string[] = [];
      page.on("request", (request) => {
        if (new URL(request.url()).pathname.endsWith("/messages") && request.method() === "POST") {
          sent.push(request.url());
        }
      });
      await page.goto(`/agents/${SESSION.session_id}/live`);
      const field = page.getByTestId("agent-composer").locator("textarea");
      await expect(field).toBeVisible();
      await walkBox(field, world);
      await expectRow(page, field, "agent-live-composer", "agent-message");
      // Enter took the row; nothing was sent by it.
      expect(sent, "Enter on a picker row sent a message").toEqual([]);
      // The `/` door is still the session's own commands, beside the three reference doors.
      await field.fill("/");
      await expect(page.getByRole("listbox", { name: "Slash commands" })).toBeVisible();
      await field.fill("");
    },
    seed: async () => {
      await publishAgentStreamFrame(
        SESSION.session_id,
        {
          frames: [
            {
              commands: [
                { description: "Compact the context", name: "compact", source: "builtin" },
              ],
              kind: "commands",
              seq: 1,
              v: 1,
            },
          ],
          session_id: SESSION.session_id,
          v: 1,
        },
        "replay"
      );
    },
  },
  "agents-row-composer": {
    drive: async (page, world) => {
      await page.goto("/agents");
      const row = agentRow(page, SESSION.session_id);
      await row.getByRole("button", { exact: true, name: SESSION.title }).click();
      const field = row.getByRole("textbox", { name: "Comment" });
      await expect(field).toBeVisible();
      await walkBox(field, world);
      // With no issue picked the write is a message to the session, which names no issue and so
      // lists everyone who has signed in.
      await expectRow(page, field, "agents-row-composer", "agent-message");
      await row.getByRole("button", { name: "Choose issue" }).click();
      await row.getByRole("combobox", { name: "Issue" }).selectOption(world.host);
      await expectRow(page, field, "agents-row-composer", "comment");
      // The reply under the message the row shows goes to that thread's target.
      await row.getByRole("button", { name: "Reply" }).first().click();
      await expectRow(page, field, "agents-row-composer", "message-reply");
    },
  },
  "approval-reason": {
    drive: async (page, world) => {
      await page.goto(`/issues/${world.host}`);
      await page.getByRole("button", { name: "Request changes" }).click();
      const dialog = page.getByRole("dialog", { name: "Request changes" });
      const field = dialog.getByRole("textbox", { name: "Reason" });
      await expect(field).toBeVisible();
      await walkBox(field, world);
      await expectRow(page, field, "approval-reason", "approval-reason");
    },
    seed: async (world) => {
      await requestApproval(
        world.spec,
        {},
        { actor: { id: SESSION.session_id, kind: "session" }, as: "agent" }
      );
    },
  },
  "architecture-reason": {
    drive: async (page, world) => {
      await page.goto("/projects/CORE/architecture");
      await page
        .getByRole("button", { name: /unassigned$/ })
        .first()
        .click();
      const unassigned = page.getByTestId("unassigned-list");
      await unassigned.getByRole("button", { name: `Not architectural…` }).first().click();
      const field = page.getByRole("textbox", { name: `Why ${world.host} is not architectural` });
      await expect(field).toBeVisible();
      await walkBox(field, world);
      await expectRow(page, field, "architecture-reason", "architecture-reason");
    },
    seed: async () => {
      await seedArchitecture();
    },
  },
  "ask-answer": {
    drive: async (page, world) => {
      await page.goto("/");
      const card = await askCard(page, world.ask);
      const field = card.locator("[data-ask-answer]");
      await expect(field).toBeVisible();
      await walkBox(field, world);
      // One box, two writes: the reader chooses there whether it answers or asks back, so both
      // rows of the list carry the one line it shows. The field itself is handed the
      // `ask-answer` row (`AskCard.tsx`, `writing={dispatchWritingBox("ask-answer",
      // "ask-answer")}`), so that row's claim is the one its party list draws.
      await expectRow(page, field, "ask-answer", "ask-answer");
      // The `ask-back` row's own words and people, which the same field draws under that
      // write: the hint both rows share is what tells the reader that asking back delivers.
      await expectHint(page, field, "ask-answer", "ask-back");
      await expectIssueRows(field, dispatchWritingBox("ask-answer", "ask-back").issueRows);
      // And that row's `delivers`, which no state of the field draws, is driven instead: a
      // question back with a party picked in it is a comment on the ask, so the party is told,
      // and the fake listener takes one frame for it carrying the body the reader wrote.
      const before = (await getSentMessages()).length;
      await keyboardOnly(field);
      await pickReference(field, `@${QUERY}`, new RegExp(`^${SESSION.title}`));
      await field.press("End");
      const back = " which window does it replay?";
      await field.pressSequentially(back);
      const body = `[${SESSION.title}](dispatch://agent/${SESSION.session_id})${back}`;
      await expect(field).toHaveValue(body);
      await card.getByRole("button", { exact: true, name: "Ask back" }).click();
      await expect
        .poll(async () => (await getSentMessages()).slice(before))
        .toMatchObject([{ message: body, target_session: SESSION.session_id }]);
    },
  },
  "ask-decline-note": {
    drive: async (page, world) => {
      await page.goto("/");
      const card = await askCard(page, world.ask);
      await card.getByRole("button", { exact: true, name: "Decline to answer" }).click();
      const dialog = page.getByRole("dialog", { name: "Decline to answer" });
      const field = dialog.getByRole("textbox", { name: "Note (optional)" });
      await expect(field).toBeVisible();
      await walkBox(field, world);
      await expectRow(page, field, "ask-decline-note", "ask-decline");
    },
  },
  "ask-option-description": {
    drive: async (page, world) => {
      await openSpec(page, world.host, ANCHOR);
      const form = await marginComposer(page, "Ask");
      const field = form.getByRole("textbox", { name: "Option 1 description" });
      await walkBox(field, world);
      await expectRow(page, field, "ask-option-description", "ask");
    },
  },
  "ask-option-label": {
    drive: async (page, world) => {
      await openSpec(page, world.host, ANCHOR);
      const form = await marginComposer(page, "Ask");
      const field = form.getByRole("textbox", { name: "Option 1 label" });
      await walkBox(field, world);
      await expectRow(page, field, "ask-option-label", "ask");
      // Enter took the picker's row rather than adding the option Enter adds here.
      await expect(form.getByRole("textbox", { name: "Option 2 label" })).toHaveCount(0);
    },
  },
  "ask-thread-reply": {
    drive: async (page, world) => {
      // The answered ask's own thread, which the Conversation draws behind `Write a reply`:
      // the reply form belongs to an ask somebody has answered (`AskReplyComposer`).
      await page.goto(`/issues/${world.host}/conversation`);
      const turn = page.locator(`[data-turn="ask:${world.ask}"]`);
      await turn.getByRole("button", { exact: true, name: "Write a reply" }).click();
      const field = turn.getByRole("textbox", { name: "Reply" });
      await expect(field).toBeVisible();
      await walkBox(field, world);
      await expectRow(page, field, "ask-thread-reply", "comment");
    },
    seed: async (world) => {
      await answerAsk(world.ask, { expected_edited_at: null, selected: ["Ship"] });
    },
  },
  broadcast: {
    drive: async (page, world) => {
      await page.goto("/agents");
      await page.getByRole("checkbox", { name: `Select ${SESSION.title} for broadcast` }).check();
      const region = page.getByRole("region", { name: "Broadcast" });
      const field = region.getByRole("textbox", { name: "Broadcast message" });
      await expect(field).toBeVisible();
      await walkBox(field, world);
      await expectRow(page, field, "broadcast", "broadcast");
    },
  },
  "conversation-composer": {
    drive: async (page, world) => {
      await page.goto(`/issues/${world.host}/conversation`);
      const field = commentField(page);
      await walkBox(field, world);
      await expectRow(page, field, "conversation-composer", "comment");
      // A Reply under a message makes the same box answer that thread's target.
      await page
        .locator(`[data-turn="message:${world.message}"]`)
        .getByRole("button", { name: "Reply" })
        .first()
        .click();
      await expectRow(page, field, "conversation-composer", "message-reply");
    },
  },
  "create-issue-spec": {
    drive: async (page, world) => {
      await page.goto("/projects/CORE");
      await page
        .getByRole("complementary", { name: "Navigation" })
        .getByRole("button", { name: "New issue" })
        .click();
      const dialog = page.getByRole("dialog", { name: "Create issue" });
      const field = dialog.getByRole("textbox", { name: "Spec (optional)" });
      await expect(field).toBeVisible();
      await walkBox(field, world);
      await expectRow(page, field, "create-issue-spec", "issue-spec");
    },
  },
  "decisions-change": {
    drive: async (page, world) => {
      await page.goto(`/issues/${world.host}/decisions`);
      await page.getByRole("button", { exact: true, name: "Change" }).first().click();
      const field = page.getByRole("textbox", { name: "Your change" });
      await expect(field).toBeVisible();
      await walkBox(field, world);
      await expectRow(page, field, "decisions-change", "comment");
    },
    seed: async (world) => {
      const decisions = await createIssueArtifact(
        world.host,
        {
          content: "## Store notices in a table\n\nChosen for the simplest storage.\n",
          name: "decisions.md",
        },
        { actor: { id: SESSION.session_id, kind: "session" }, as: "agent" }
      );
      expect(decisions.artifact.slug).toBe("decisions-md");
    },
  },
  "document-body": {
    drive: async (page, world) => {
      // The document's other reader, open before the pick: what the editor writes reaches them
      // through the collaboration socket, as it reaches the document's stored text.
      const other = await page.context().newPage();
      await openSpec(other, world.host, ANCHOR);

      await openSpec(page, world.host, ANCHOR);
      const editor = documentEditor(page);
      await expectHint(page, editor, "document-body", "document-text");
      const list = refPicker(page);
      /** Clears the line the caret is on, picker and all. */
      const clearLine = async () => {
        await page.keyboard.press("Escape");
        await page.keyboard.press("Shift+Home");
        await page.keyboard.press("Backspace");
        await expect(list).toHaveCount(0);
      };
      await editor.click();
      await keyboardOnly(editor);
      await page.keyboard.press("Control+End");
      await page.keyboard.press("Enter");
      for (const door of ["[[", "@", "#"] as const) {
        await page.keyboard.type(`${door}${QUERY}`);
        await expect
          .poll(() => pickerRows(list), { message: `the ${door} door's rows` })
          .toEqual(DOOR_ROWS[door]);
        await clearLine();
      }
      await page.keyboard.type(`[[${NO_MATCH}`);
      await expect(list.getByTestId("ref-picker-empty")).toHaveText(REF_PICKER_WORDS.empty);
      await clearLine();
      // The document belongs to an issue, so the picker offers that issue's people alone.
      await page.keyboard.type(`@${OFF_ISSUE_PERSON}`);
      await expect(list.getByTestId("ref-picker-empty")).toHaveText(REF_PICKER_WORDS.empty);
      await clearLine();

      // The pick lands at the caret, written through the editor's own transaction.
      await page.keyboard.type(" then deploy");
      await page.keyboard.press("Home");
      await page.keyboard.type(`Ship after #${QUERY}`);
      await expect.poll(() => pickerRows(list)).toEqual(DOOR_ROWS["#"]);
      await page.keyboard.press("Enter");
      const written = `Ship after dispatch://${world.target} then deploy`;
      await expect
        .poll(async () => (await getArtifactText(world.spec)).markdown, { timeout: 30_000 })
        .toContain(written);
      // The other reader has it, drawn by the name of what it names.
      await expect(documentEditor(other)).toContainText(`Ship after ${world.target} Zephyr`);

      // A party picked with `@` is stored as the link that names it, not as the characters of
      // one: a document is read far outside this dashboard - `dispatch read`, an export, the Go
      // renderer - and only the link carries the party's name there (R4). The earlier shape
      // wrote the markdown as text, so the stored document held `\[Zephyr bot]\(dispatch://…)`
      // and every reader saw the brackets.
      await page.keyboard.press("Control+End");
      await page.keyboard.press("Enter");
      await page.keyboard.type(`Ask @${QUERY}`);
      await expect.poll(() => pickerRows(list)).toEqual(DOOR_ROWS["@"]);
      await page.keyboard.press("Enter");
      const mention = `Ask [${SESSION.title}](dispatch://agent/${SESSION.session_id})`;
      await expect
        .poll(async () => (await getArtifactText(world.spec)).markdown, { timeout: 30_000 })
        .toContain(mention);
      // And the other reader sees the party's name, with no bracket of the address it carries.
      await expect(documentEditor(other)).toContainText(`Ask ${SESSION.title}`);
      await expect(documentEditor(other)).not.toContainText("](dispatch://agent/");
      await other.close();
    },
  },
  "inbox-thread-reply": {
    drive: async (page, world) => {
      await page.goto("/");
      await selectRow(page, replyRowId(world.comment));
      const field = inboxPane(page).getByRole("textbox", { name: "Reply" });
      await expect(field).toBeVisible();
      await walkBox(field, world);
      await expectRow(page, field, "inbox-thread-reply", "comment");
      await selectRow(page, replyRowId(world.message));
      const onMessage = inboxPane(page).getByRole("textbox", { name: "Reply" });
      await expect(onMessage).toBeVisible();
      await expectRow(page, onMessage, "inbox-thread-reply", "message-reply");
    },
  },
  "margin-comment-edit": {
    drive: async (page, world) => {
      await openSpec(page, world.host, ANCHOR);
      const card = await openMarginCard(page, world.anchored);
      await card.getByRole("button", { exact: true, name: "Edit" }).click();
      const field = card.getByRole("textbox", { name: "Edit comment" });
      await expect(field).toBeVisible();
      await walkBox(field, world);
      await expectRow(page, field, "margin-comment-edit", "comment-edit");
    },
  },
  "margin-composer": {
    drive: async (page, world) => {
      await openSpec(page, world.host, ANCHOR);
      const comment = await marginComposer(page, "Comment");
      const body = comment.getByRole("textbox", { exact: true, name: "Comment" });
      await walkBox(body, world);
      await expectRow(page, body, "margin-composer", "comment");
      await page.keyboard.press("Escape");

      const suggestion = await marginComposer(page, "Suggest");
      const reason = suggestion.getByRole("textbox", { name: "Reason" });
      await expectRow(page, reason, "margin-composer", "suggestion");
      await page.keyboard.press("Escape");

      const ask = await marginComposer(page, "Ask");
      const question = ask.getByRole("textbox", { name: "Question" });
      await expectRow(page, question, "margin-composer", "ask");
    },
  },
  "margin-thread-reply": {
    drive: async (page, world) => {
      await openSpec(page, world.host, ANCHOR);
      const card = await openMarginCard(page, world.anchored);
      const field = card
        .getByRole("form", { name: "Comment composer" })
        .getByRole("textbox", { name: "Reply" });
      await expect(field).toBeVisible();
      await walkBox(field, world);
      await expectRow(page, field, "margin-thread-reply", "comment");
    },
  },
  "phone-thread-composer": {
    drive: async (page, world) => {
      await page.setViewportSize({ height: 844, width: 390 });
      await page.goto(`/issues/${world.host}/conversation`);
      const card = page.locator(`[data-turn="comment:${world.comment}"]`);
      await card.getByRole("button", { name: "Expand thread" }).click();
      const thread = page.getByRole("dialog", { name: "Thread" });
      await expect(thread).toBeVisible();
      await thread.getByRole("button", { name: "Reply" }).first().click();
      const field = commentField(page);
      await expect(field).toBeVisible();
      await walkBox(field, world);
      await expectRow(page, field, "phone-thread-composer", "comment");
    },
  },
  "suggestion-replacement": {
    drive: async (page, world) => {
      await openSpec(page, world.host, ANCHOR);
      const form = await marginComposer(page, "Suggest");
      const field = form.getByRole("textbox", { name: "Replacement" });
      await expect(field).toBeVisible();
      await walkBox(field, world);
      await expectRow(page, field, "suggestion-replacement", "suggestion");
    },
  },
};

for (const box of coveredBoxes()) {
  // SLOW(>=2s): each row seeds the world - a project, two issues, a document, a live session and
  // the threads its box answers - loads the page its box is on, and opens the picker seven times,
  // each open its own read of the registry, the issue's people and the search route.
  test(`${box}: three doors over the same rows, the same words, a pick at the caret and its own hint`, async ({
    browser,
  }) => {
    // Both directions at once: a covered box with no driver fails here, and a driver for a box
    // the list does not cover fails every row.
    expect(Object.keys(DRIVERS).sort(), "the suite's boxes and the list's covered boxes").toEqual(
      [...coveredBoxes()].sort()
    );
    const driver = DRIVERS[box];
    if (driver === undefined) throw new Error(`no driver for ${box}`);
    expect(rowsOfBox(box).length, `${box} has no covered row`).toBeGreaterThan(0);

    const world = await seedWritingBoxes();
    await driver.seed?.(world);
    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      await driver.drive(page, world);
    } finally {
      await alice.close();
    }
  });
}
