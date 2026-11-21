import { setLiveSessions } from "./agents";
import { editAsk, getArtifactText, getIssueEvents, replyToMessageDelivery } from "./api";
import {
  askNow,
  asTester,
  badgeOf,
  callsOf,
  FIRST_LINE,
  fixtureQuestion,
  framesTo,
  type Hand,
  keptRewrite,
  MARKUP_OPTION_LINE,
  openSurface,
  PICTURE_LINE,
  QUESTION,
  REF_LINE,
  REWRITE_PROMPT,
  REWRITTEN,
  RUN_LINK,
  rewriteEditOf,
  rewriteRule,
  rewriteText,
  SURFACES,
  type Surface,
  seedProject,
  seedSurface,
  TESTER,
  tester,
} from "./ask-card";
import { modelRequests, setModelRules } from "./fake-model-helpers";
import { inboxPane, inboxRow } from "./inbox-pane";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * Rewrite (INFRA-3231's R7, check 5106bd80): the rows of INFRA-3552's check c1 on each of its six
 * surfaces, with the check's changes. The Rewrite route answers at once and the card shows the
 * badge Rewriting… while the rewrite runs, then the new wording in place under the badge
 * Rewritten and no other text about the rewrite; the badge and `w` show the original wording in
 * place under the badge Original, and again the rewrite; a decision block shows the original
 * question and options in its card while the document keeps the rewrite; a rewrite that fails or
 * is not applied shows Rewrite failed with its reason as the badge's tooltip alone, and the badge
 * or `r` tries again; the stored ask.edited says who asked, why and whether the asking agent's
 * answer was read; the ask's read carries the rewrite; and an answer to the old wording is
 * refused while the card says that the question changed.
 *
 * The stand-in model (`e2e/fake-model.ts`) writes every rewrite, scripted per row. The tester's
 * session is in the fake Envoy only in the row that reads its side question: anywhere else the
 * listener knows no such session, so a rewrite reads Dispatch's record alone and waits for
 * nobody.
 *
 * At the commit before the change this file fails: `r` does nothing and the card has no Rewrite.
 */

/** Words that would say something of the rewrite besides its badge: who asked, what it read,
 *  why. The card says none of them. */
const REWRITE_TALK = /rewrote|rewritten at|at your request|record alone|agent's answer|Escape/i;

/** Presses `r` on the card in hand and waits for the route's answer, which comes while the
 *  stand-in still holds the rewrite. */
async function pressRewrite(page: Page, hand: Hand, askId: string): Promise<void> {
  await hand.grab();
  const answered = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      new URL(response.url()).pathname === `/api/v1/asks/${askId}/rewrites`
  );
  await page.keyboard.press("r");
  const response = await answered;
  expect(response.status()).toBe(202);
  const answer: unknown = await response.json();
  expect(answer).toMatchObject({ rewrite: { state: "rewriting" } });
}

/** The whole of c1's first rows on one surface: Rewrite offered with its key, `r` answered at
 *  once with the badge Rewriting…, the new wording in place under Rewritten, the original one key
 *  and one press away, the read carrying the rewrite and its original, and the quiet event
 *  holding who asked. */
async function rewriteOn(page: Page, surface: Surface): Promise<void> {
  await seedProject();
  const seeded = await seedSurface(surface);
  const original = (await askNow(seeded.askId)).question;
  await setModelRules([rewriteRule(await keptRewrite(seeded.askId), { delayMs: 1_500 })]);
  const hand = await openSurface(page, surface, seeded);
  await expect(hand.question).toContainText(QUESTION);

  // The menu offers Rewrite, under its key, and no badge stands before a rewrite.
  await hand.grab();
  await hand.card.locator("[data-ask-menu-button]").click();
  const item = page.locator('[data-ask-menu] [data-ask-menu-item="rewrite"]');
  await expect(item).toContainText("Rewrite");
  await expect(item.locator("kbd")).toHaveText("r");
  await page.keyboard.press("Escape");
  await expect(page.locator("[data-ask-menu]")).toHaveCount(0);
  await expect(badgeOf(hand.frame)).toHaveCount(0);

  await pressRewrite(page, hand, seeded.askId);
  const badge = badgeOf(hand.frame);
  await expect(badge).toHaveText("Rewriting…");
  await expect(badge).toHaveText("Rewritten", { timeout: 20_000 });
  await expect(hand.question).toContainText(REWRITTEN);
  await expect(hand.question).not.toContainText(QUESTION);
  await expect(hand.frame).not.toContainText(REWRITE_TALK);

  // `w` shows the original in place, under Original, and again the rewrite; so does the badge.
  await hand.grab();
  await page.keyboard.press("w");
  await expect(badge).toHaveText("Original");
  if (surface === "decision block") {
    // The block's card holds the agent's wording; the document's own text stays the rewrite.
    await expect(hand.frame).toHaveAttribute("data-ask-original", "");
    await expect(hand.card.locator("[data-ask-question]")).toContainText(QUESTION);
    await expect(hand.question).toBeHidden();
    expect((await getArtifactText(seeded.artifactId)).markdown).toContain(REWRITTEN);
  } else {
    await expect(hand.question).toContainText(QUESTION);
  }
  await hand.grab();
  await page.keyboard.press("w");
  await expect(badge).toHaveText("Rewritten");
  await expect(hand.question).toContainText(REWRITTEN);
  await badge.click();
  await expect(badge).toHaveText("Original");
  await badge.click();
  await expect(badge).toHaveText("Rewritten");

  // The read carries the rewrite, the person's, with the wording it replaced.
  const rewrite = (await askNow(seeded.askId)).rewrite;
  expect(rewrite).toMatchObject({ by: "person", original: { body: original }, state: "rewritten" });
  // The stored ask.edited is Dispatch's own and quiet, names alice as who asked, and says the
  // asking agent's answer was not read: no session took a side question.
  const edited = rewriteEditOf(await getIssueEvents(seeded.issueKey), seeded.askId);
  expect(edited?.actor.kind).toBe("system");
  expect(edited?.notify).toBe(false);
  expect(edited?.payload.quiet).toBe(true);
  expect(edited?.payload.previous?.question).toBe(original);
  expect(edited?.payload.rewrite).toMatchObject({
    agent_answer: false,
    asked_by: { id: "alice" },
    by: "person",
  });
}

for (const surface of SURFACES) {
  // SLOW(>=2s): a seed, a page, and a rewrite the stand-in holds for 1.5 s so the badge can be
  // read while it runs.
  test(`Rewrite on the ${surface} rewrites the question in place under its badge, the original a key away`, async ({
    browser,
  }) => {
    const alice = await asUser(browser, "alice");
    try {
      await rewriteOn(await alice.newPage(), surface);
    } finally {
      await alice.close();
    }
  });
}

// SLOW(>=2s): a decision block's rewrite writes a new version of its document through the room.
test("a decision block's rewrite is a new version that keeps its option line holding markup", async ({
  browser,
}) => {
  await seedProject();
  const seeded = await seedSurface("decision block");
  const before = await getArtifactText(seeded.artifactId);
  expect(before.markdown).toContain(MARKUP_OPTION_LINE);
  const optionIds = ((await askNow(seeded.askId)).options ?? []).map((option) => option.id ?? "");
  await setModelRules([rewriteRule(await keptRewrite(seeded.askId))]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const hand = await openSurface(page, "decision block", seeded);
    await pressRewrite(page, hand, seeded.askId);
    await expect(badgeOf(hand.frame)).toHaveText("Rewritten", { timeout: 20_000 });
    await expect
      .poll(async () => {
        const after = await getArtifactText(seeded.artifactId);
        return (after.version ?? 0) > (before.version ?? 0) && after.markdown.includes(REWRITTEN);
      })
      .toBe(true);
    const after = await getArtifactText(seeded.artifactId);
    expect(after.markdown).toContain(MARKUP_OPTION_LINE);
    for (const kept of [FIRST_LINE, REF_LINE, PICTURE_LINE, RUN_LINK]) {
      expect(after.markdown).toContain(kept);
    }
    // Each option keeps the id it had.
    expect(((await askNow(seeded.askId)).options ?? []).map((option) => option.id ?? "")).toEqual(
      optionIds
    );
  } finally {
    await alice.close();
  }
});

/** One break of what a rewrite keeps: how the stand-in breaks the question, and the reason the
 *  badge's tooltip then gives. */
interface Break {
  readonly name: string;
  readonly reason: string;
  readonly reply: (
    question: string,
    options: readonly { label: string; id?: string; description?: string }[]
  ) => string;
}

const BREAKS: readonly Break[] = [
  {
    name: "first line",
    reason: "the rewrite changes the first line",
    reply: (question, options) =>
      rewriteText(
        question.replace(FIRST_LINE, "A finding from the tester").replace(QUESTION, REWRITTEN),
        options
      ),
  },
  {
    name: "Ref line",
    reason: "the rewrite drops or changes the line",
    reply: (question, options) =>
      rewriteText(question.replace(`\n${REF_LINE}`, "").replace(QUESTION, REWRITTEN), options),
  },
  {
    name: "picture line",
    reason: "the rewrite drops or changes the picture line",
    reply: (question, options) =>
      rewriteText(
        question
          .replace(PICTURE_LINE, "The chart shows the flip rate.")
          .replace(QUESTION, REWRITTEN),
        options
      ),
  },
  {
    name: "link",
    reason: "the rewrite drops the link",
    reply: (question, options) =>
      rewriteText(
        question.replace(` ${RUN_LINK}`, " the run").replace(QUESTION, REWRITTEN),
        options
      ),
  },
  {
    name: "label",
    reason: "has the label",
    reply: (question, options) =>
      rewriteText(question.replace(QUESTION, REWRITTEN), [
        options[0],
        { ...options[1], label: "Move it first" },
      ]),
  },
  {
    name: "count",
    reason: "the rewrite has 1 option",
    reply: (question, options) => rewriteText(question.replace(QUESTION, REWRITTEN), [options[0]]),
  },
  {
    name: "order",
    reason: "the rewrite puts the options in the order",
    reply: (question, options) =>
      rewriteText(question.replace(QUESTION, REWRITTEN), [options[1], options[0]]),
  },
  {
    name: "choice",
    reason: "multiple-choice",
    reply: (question, options) => rewriteText(question.replace(QUESTION, REWRITTEN), options, true),
  },
];

// SLOW(>=2s): eight rewrites the stand-in breaks, each read back on its own card, then two tries
// again.
test("a rewrite that breaks what a rewrite keeps leaves the ask as it was and says why in its badge's tooltip alone", async ({
  browser,
}) => {
  await seedProject();
  const seeds = [];
  for (const broken of BREAKS) {
    // Each break on an issue of its own, its marker in the question, so the stand-in tells the
    // rewrites apart by the record each request reads.
    const question = `${QUESTION} (${broken.name} case)`;
    const seeded = await seedSurface("reading pane", {
      question,
      title: `Flip audit ${broken.name}`,
    });
    const ask = await askNow(seeded.askId);
    seeds.push({ ask, broken, question, seeded });
  }
  await setModelRules(
    seeds.map(({ ask, broken, question }) =>
      rewriteRule(broken.reply(ask.question, ask.options ?? []), { contains: question })
    )
  );
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    for (const { broken, question, seeded } of seeds) {
      const hand = await openSurface(page, "reading pane", seeded);
      await pressRewrite(page, hand, seeded.askId);
      const badge = badgeOf(hand.frame);
      await expect(badge, broken.name).toHaveText("Rewrite failed", { timeout: 20_000 });
      await expect(badge, broken.name).toHaveAttribute(
        "title",
        new RegExp(escapeRegExp(broken.reason))
      );
      await expect(hand.frame, broken.name).not.toContainText(broken.reason);
      await expect(hand.question, broken.name).toContainText(question);
      expect((await askNow(seeded.askId)).question, broken.name).toBe(fixtureQuestion(question));
      expect((await askNow(seeded.askId)).rewrite, broken.name).toMatchObject({ state: "failed" });
    }

    // The badge tries again, and so does `r`; a rewrite that is applied keeps each option's id.
    const [first, second] = seeds;
    const ids = (first.ask.options ?? []).map((option) => option.id ?? "");
    await setModelRules([
      rewriteRule(await keptRewrite(first.seeded.askId), { contains: first.question }),
      rewriteRule(await keptRewrite(second.seeded.askId), { contains: second.question }),
    ]);
    let hand = await openSurface(page, "reading pane", first.seeded);
    await badgeOf(hand.frame).click();
    await expect(badgeOf(hand.frame)).toHaveText("Rewritten", { timeout: 20_000 });
    expect(
      ((await askNow(first.seeded.askId)).options ?? []).map((option) => option.id ?? "")
    ).toEqual(ids);
    hand = await openSurface(page, "reading pane", second.seeded);
    await pressRewrite(page, hand, second.seeded.askId);
    await expect(badgeOf(hand.frame)).toHaveText("Rewritten", { timeout: 20_000 });
  } finally {
    await alice.close();
  }
});

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// SLOW(>=2s): the stand-in holds the rewrite for 2.5 s, through which the second press is made.
test("a second r while a rewrite runs makes no second model call and shows the same result", async ({
  browser,
}) => {
  await seedProject();
  const seeded = await seedSurface("reading pane");
  await setModelRules([rewriteRule(await keptRewrite(seeded.askId), { delayMs: 2_500 })]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const hand = await openSurface(page, "reading pane", seeded);
    await pressRewrite(page, hand, seeded.askId);
    await expect(badgeOf(hand.frame)).toHaveText("Rewriting…");
    await hand.grab();
    await page.keyboard.press("r");
    await expect(badgeOf(hand.frame)).toHaveText("Rewritten", { timeout: 20_000 });
    await expect(hand.question).toContainText(REWRITTEN);
    expect(await callsOf(REWRITE_PROMPT)).toBe(1);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the stand-in holds the rewrite for 3 s, inside which the agent edits its question.
test("an agent's edit while the rewrite runs keeps the agent's wording, and the badge says the rewrite was not applied", async ({
  browser,
}) => {
  await seedProject();
  const seeded = await seedSurface("reading pane");
  await setModelRules([rewriteRule(await keptRewrite(seeded.askId), { delayMs: 3_000 })]);
  const agentsWording = fixtureQuestion("Should the flip audit run before the main ledger moves?");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const hand = await openSurface(page, "reading pane", seeded);
    await pressRewrite(page, hand, seeded.askId);
    await expect.poll(() => callsOf(REWRITE_PROMPT)).toBe(1);
    await editAsk(seeded.askId, { question: agentsWording }, asTester);
    const badge = badgeOf(hand.frame);
    await expect(badge).toHaveText("Rewrite failed", { timeout: 20_000 });
    await expect(badge).toHaveAttribute(
      "title",
      /the asking agent changed the question while it was rewritten/
    );
    await expect(hand.question).toContainText("before the main ledger moves");
    expect((await askNow(seeded.askId)).question).toBe(agentsWording);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the answer is held in the network until the rewrite it was not sent against lands.
test("an answer sent against the old wording is refused, and the card says that the question changed", async ({
  browser,
}) => {
  await seedProject();
  const seeded = await seedSurface("reading pane");
  await setModelRules([rewriteRule(await keptRewrite(seeded.askId), { delayMs: 1_500 })]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const hand = await openSurface(page, "reading pane", seeded);
    // The answer leaves while the card still shows the old wording, and reaches Dispatch only
    // once the rewrite is in place: a slow network, which is how an answer meets a rewrite.
    await page.route(`**/api/v1/asks/${seeded.askId}/answer`, async (route) => {
      await expect
        .poll(async () => (await askNow(seeded.askId)).rewrite?.state, { timeout: 20_000 })
        .toBe("rewritten");
      await route.continue();
    });
    await pressRewrite(page, hand, seeded.askId);
    const refused = page.waitForResponse(
      (response) => new URL(response.url()).pathname === `/api/v1/asks/${seeded.askId}/answer`
    );
    await hand.card.locator("[data-ask-option]").first().check();
    await hand.card.locator("[data-ask-action]").click();
    expect((await refused).status()).toBe(409);
    await expect(hand.card).toContainText("This question changed while you were answering");
    expect((await askNow(seeded.askId)).state).toBe("open");
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the side question goes through the fake Envoy, and the tester answers it.
test("a rewrite asks the live asking session one side question and reads its answer", async ({
  browser,
}) => {
  await seedProject();
  const seeded = await seedSurface("reading pane");
  await setLiveSessions([TESTER]);
  const answer = "The flip audit is the nightly check of the ledger's mirror.";
  await setModelRules([rewriteRule(await keptRewrite(seeded.askId))]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const hand = await openSurface(page, "reading pane", seeded);
    await pressRewrite(page, hand, seeded.askId);
    let question = "";
    await expect
      .poll(async () => {
        const asked = (await framesTo(TESTER.session_id)).filter(
          (frame) => frame.actor.kind === "system"
        );
        question = asked[0]?.id ?? "";
        return asked.length;
      })
      .toBe(1);
    await replyToMessageDelivery(question, { attempt: 1, body: answer }, tester);
    await expect(badgeOf(hand.frame)).toHaveText("Rewritten", { timeout: 20_000 });
    const rewrites = (await modelRequests()).filter((request) =>
      request.text.includes(REWRITE_PROMPT)
    );
    expect(rewrites).toHaveLength(1);
    expect(rewrites[0]?.text).toContain(answer);
    const edited = rewriteEditOf(await getIssueEvents(seeded.issueKey), seeded.askId);
    expect(edited?.payload.rewrite?.agent_answer).toBe(true);
    // The side question names no issue: the Inbox lists no row of its own for it.
    await expect(inboxPane(page)).toBeVisible();
    await expect(inboxRow(page, question)).toHaveCount(0);
  } finally {
    await alice.close();
  }
});
