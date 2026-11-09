import { expect, type Locator, type Page, test } from "@playwright/test";

import { getSentMessages, plannerMention, plannerSession, setLiveSessions } from "./agents";
import { createComment, createIssue, createProject, listComments } from "./api";
import { commentField, pickReference } from "./ref-picker";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * INFRA-1702's `c-mention-is-a-reference`: a mention is a reference in the text, and nothing
 * beside it.
 *
 * What a pick writes is the link that names the party (R4), so the write carries no mentions
 * array at all; the server reads the party out of the body, stores it, delivers to it, and
 * every reader sees the party named. The refusals a mentions array earned are the ones a body
 * earns now, keyed on the same party set (`validateCommentMentions`): one party named twice is
 * one delivery, more delivering parties than the cap is `MENTION_INPUT`, and a body whose only
 * party is a person - whom nothing is delivered to - is written rather than refused, with the
 * mode the writer typed.
 */

/** A role nobody holds, written as a link; the server reads `role:<token>` out of it whether or
 *  not a session holds that role, which is what the cap counts. */
const role = (token: string) => `[${token}](dispatch://role/${token})`;

/** Sends the draft and answers the comment route's own answer. */
async function send(page: Page, issueKey: string) {
  const answer = page.waitForResponse(
    (candidate) =>
      candidate.request().method() === "POST" &&
      new URL(candidate.url()).pathname === `/api/v1/issues/${issueKey}/comments`
  );
  await page
    .getByRole("form", { name: "Comment composer" })
    .getByRole("button", { exact: true, name: "Send" })
    .click();
  return await answer;
}

test.beforeEach(async () => {
  await resetDatabase();
});

// SLOW(>=2s): the pick's own read of the registry, the send, and the delivery the fake listener
// takes before the stored comment can be read back.
test("a picked party is the text's own reference: no mentions array, and the comment it stores names, delivers to and shows that party", async ({
  browser,
}) => {
  await setLiveSessions([plannerSession]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "A mention is a reference" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const field = commentField(page);
    await pickReference(field, "@Planner", /^Planner\b/);
    await expect(field).toHaveValue(plannerMention);
    await field.press("End");
    await field.pressSequentially(" please look.");

    const answer = await send(page, issue.key);
    // The whole write: the body, and the mode the composer's rule gives for this session. No
    // mentions array goes with it - the party is in the text.
    expect(answer.status()).toBe(201);
    expect(answer.request().postDataJSON()).toEqual({
      body: `${plannerMention} please look.`,
      delivery: "aside",
    });

    // The server read the party out of the body, stored it, and delivered to it once.
    await expect
      .poll(() => listComments(issue.key))
      .toMatchObject([
        {
          body: `${plannerMention} please look.`,
          mentions: [{ target: `session:${plannerSession.session_id}` }],
        },
      ]);
    await expect
      .poll(() => getSentMessages())
      .toMatchObject([{ target_session: plannerSession.session_id }]);

    // And every reader sees the party named in the text itself, as a link into its own live
    // conversation. Taken from the turn's body: the card also lists the delivery the mention
    // made, whose own link names the same session.
    const turn = page
      .getByRole("list", { name: "Conversation turns" })
      .locator(":scope > li")
      .filter({ hasText: "please look." });
    const named = turn
      .locator("[data-markdown-preview], [data-markdown-body]")
      .getByRole("link", { exact: true, name: plannerSession.title });
    await expect(named).toHaveAttribute("href", `/agents/${plannerSession.session_id}/live`);
    await expect(named).toHaveAttribute(
      "data-dispatch-ref",
      `dispatch://agent/${plannerSession.session_id}`
    );
  } finally {
    await alice.close();
  }
});

/** A comment's Conversation card, expanded: Edit lives inside the thread, so a collapsed card
 *  offers none - every editing row of `e2e/conversation.e2e.ts` expands the card first. */
async function expandedComment(page: Page, id: string): Promise<Locator> {
  const card = page.locator(`[data-turn="comment:${id}"]`);
  await card.getByRole("button", { name: "Expand thread" }).click();
  await expect(card.getByRole("button", { exact: true, name: "Edit" })).toBeVisible();
  return card;
}

// SLOW(>=2s): two saves of one comment, each of which the page reads back, with the listener
// watched across both.
test("an edit adds and removes a party in its text and sends no frame for either", async ({
  browser,
}) => {
  await setLiveSessions([plannerSession]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Edited mentions" });
  const comment = await createComment(issue.key, { body: "Nobody is named here." });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const card = await expandedComment(page, comment.id);
    const named = card.getByRole("link", { exact: true, name: plannerSession.title });

    const edit = async (body: string) => {
      await card.getByRole("button", { exact: true, name: "Edit" }).click();
      const field = card.getByRole("textbox", { name: "Edit comment" });
      await field.fill(body);
      await card.getByRole("button", { exact: true, name: "Save" }).click();
      await expect(field).toHaveCount(0);
    };

    // An edit that adds the party: the party is in the stored text from then on and every
    // reader of the record sees it named, and nothing is sent. The stored `mentions` stay
    // empty because they are the delivery ledger and an edit delivers nothing
    // (`comment_body_mentions_test.go`'s TestCommentEditChangesWhoItNamesAndSendsNothing).
    await edit(`${plannerMention} is named now.`);
    await expect
      .poll(() => listComments(issue.key))
      .toMatchObject([{ body: `${plannerMention} is named now.`, deliveries: [], mentions: [] }]);
    await expect(named).toHaveAttribute("href", `/agents/${plannerSession.session_id}/live`);
    expect(await getSentMessages()).toEqual([]);

    // And an edit that takes it out again: the text names nobody, the record shows nobody, and
    // still nothing is sent.
    await edit("Nobody is named here, after all.");
    await expect
      .poll(() => listComments(issue.key))
      .toMatchObject([{ body: "Nobody is named here, after all.", mentions: [] }]);
    await expect(named).toHaveCount(0);
    expect(await getSentMessages()).toEqual([]);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): three sends from one page, the first of which waits on a delivery.
test("one party named twice delivers once, eleven are refused, and a person alone is written and delivered to nobody", async ({
  browser,
}) => {
  await setLiveSessions([plannerSession]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "What a party set earns" });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/conversation`);
    const field = commentField(page);

    // Named twice is one party: the comment is taken and the session hears it once.
    await field.fill(`${plannerMention} and ${plannerMention} again.`);
    expect((await send(page, issue.key)).status()).toBe(201);
    await expect
      .poll(() => getSentMessages())
      .toMatchObject([{ target_session: plannerSession.session_id }]);

    // Eleven delivering parties is one over the cap, refused with the code and the reason the
    // mentions array has always been refused with.
    await field.fill(
      Array.from({ length: 11 }, (_value, index) => role(`cap-role-${index + 1}`)).join(" ")
    );
    const refused = await send(page, issue.key);
    expect(refused.status()).toBe(400);
    expect(await refused.json()).toMatchObject({
      code: "MENTION_INPUT",
      error: "mentions allow at most 10 targets",
    });
    await expect(page.getByText("mentions allow at most 10 targets")).toBeVisible();
    // The refusal hands the draft back and keeps it, with Retry beside it; the next send is
    // simply the next draft, as a reader writing on would make it.

    // A person is a party nothing is delivered to, so a body naming one and a mode typed in
    // front of it is written and reaches nobody - never refused for naming no recipient.
    await field.fill("/btw [Dana](dispatch://person/dana@example.test) take a look.");
    const person = await send(page, issue.key);
    expect(person.status()).toBe(201);
    expect(person.request().postDataJSON()).toEqual({
      body: "[Dana](dispatch://person/dana@example.test) take a look.",
      delivery: "btw",
    });
    expect(
      (await getSentMessages()).filter((sent) => sent.target_session !== plannerSession.session_id)
    ).toEqual([]);
  } finally {
    await alice.close();
  }
});
