/**
 * Every row here reads what Dispatch's outbox published on each session's agent subject, which
 * the fake Envoy listener reads off its stack's own JetStream server (`GET /__fixture/bus-frames`),
 * and the outbox runs only for a server that has that bus; so these rows run in `chromium-bus`
 * alone, whose stack starts one (e2e/harness-bus.ts), and a recording of the API could hold
 * none of what they read.
 *
 * What they hold is R14 of dispatch://INFRA-341, check c-a14's browser part: a person's answer to
 * an ask goes to each agent its text names, as well as to whoever asked (the answer route
 * records the parties on `ask.answered` as `named_targets`, and the outbox's `addAnswerParties`
 * sends the event to each named session on that session's own subject); an agent the answer
 * named that replies in the ask's thread follows the ask from then on, and gets the next reply
 * there (`addFollowerRoutes`); and the ask card's box of the writing list, whose answer is held
 * to the copy its party gets on the bus, run with the driver `e2e/writing-surfaces.e2e.ts` runs
 * every other box with (`BUS_BOXES`, e2e/writing-boxes.ts).
 *
 * A stack with no bus answers the read of the frames 409, and `getBusFrames` throws on it, so a
 * row here on such a stack fails, at its first line, and never passes by reading no frame.
 */
import { type FakeSession, getBusEvents, getBusFrames, setLiveSessions } from "./agents";
import { createAsk, createComment, createIssue, createProject, listSessionAnswers } from "./api";
import { askCard } from "./inbox-pane";
import { watchWrites } from "./keyed-writes";
import { expectNoneOf } from "./none-of";
import { pickReference } from "./ref-picker";
import { sessionIds } from "./session-ids";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";
import { BUS_BOXES } from "./writing-boxes";
import { driveWritingBox } from "./writing-drivers";

/** The agent a person picks in the answer box. */
const PICKED = {
  capabilities: ["steer"],
  dir: "/workspaces/planner",
  machine_id: "e2e",
  roles: [],
  session_id: sessionIds.planner,
  title: "Planner",
} satisfies FakeSession;

/** The session that asks: another than the one the answer names, so the copy the picked agent
 *  gets is the one its name in the answer sends it, never the one the ask's follower route sends
 *  an asker. */
const ASKER = {
  ...PICKED,
  dir: "/workspaces/reviewer",
  session_id: sessionIds.reviewer,
  title: "Reviewer",
} satisfies FakeSession;

/** A live session the answer does not name and that does not follow the ask, which no copy of
 *  the answer may reach. */
const BYSTANDER = {
  ...PICKED,
  dir: "/workspaces/observer",
  session_id: sessionIds.observer,
  title: "Observer",
} satisfies FakeSession;

const PICKED_MENTION = `[@${PICKED.title}](dispatch://agent/${PICKED.session_id})`;

/** The answer the person writes, naming the picked agent. */
const ANSWER = `Over to ${PICKED_MENTION} for the rollout.`;

/** An issue with one open ask on it, asked by `ASKER`, and the three sessions live. */
async function seedAsk() {
  await setLiveSessions([PICKED, ASKER, BYSTANDER]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Rollout plan" });
  const ask = await createAsk(
    issue.key,
    { question: "Who takes the rollout?" },
    { actor: { id: ASKER.session_id, kind: "session" }, as: "agent" }
  );
  return { ask, issue };
}

/** The copies of the answer to `askId` the bus carried, for `session` alone when one is named:
 *  each `ask.answered` frame of that ask, as the session it reached, whether its envelope names
 *  the event it carries, whether the answer's text holds the picked agent's reference, and the
 *  parties the answer route recorded on the event. */
async function answerCopies(askId: string, session?: string) {
  return (await getBusEvents(session))
    .filter((event) => event.type === "ask.answered" && event.payload?.id === askId)
    .map((event) => {
      const text = (event.payload?.answer as { text?: unknown } | null | undefined)?.text;
      return {
        holdsReference:
          typeof text === "string" && text.includes(`dispatch://agent/${PICKED.session_id}`),
        sameEvent: event.envelopeEventId === `dispatch-${String(event.id)}`,
        session: event.session,
        targets: event.payload?.named_targets ?? [],
      };
    });
}

/** The replies in the thread of `askId` the bus carried to `session`, by comment id. */
async function threadReplies(askId: string, session: string): Promise<unknown[]> {
  return (await getBusEvents(session))
    .filter((event) => event.type === "comment.created" && event.payload?.ask_id === askId)
    .map((event) => event.payload?.id);
}

/** Whether `session`'s record of what people wrote to it holds the item `kind` `id`: the
 *  outbox's record of having published it on the session's own subject, written once NATS
 *  acknowledged every publish of its batch (`listSessionAnswers`). */
async function recordedFor(session: string, kind: "answer" | "comment", id: string) {
  return ((await listSessionAnswers(session)).items ?? []).some(
    (item) => item.kind === kind && item.id === id
  );
}

/** A person opens the ask in the Inbox, types `@` in its answer box, picks `PICKED`, writes the
 *  rest of `ANSWER` and presses Answer; then the copy the picked agent gets is read off the bus
 *  until it is there, read again once the outbox has recorded it, and held to one. */
async function answerNamingThePickedAgent(page: Page, askId: string): Promise<void> {
  const answers = watchWrites(page, "POST", `/api/v1/asks/${askId}/answer`);
  await page.goto("/");
  const card = await askCard(page, askId);
  const field = card.getByLabel("Your answer");
  await pickReference(field, "Over to @", /^Planner\b/);
  await expect(field).toHaveValue(`Over to ${PICKED_MENTION}`);
  await field.press("End");
  await field.pressSequentially(" for the rollout.");
  await expect(field).toHaveValue(ANSWER);
  await card.getByRole("button", { exact: true, name: "Answer" }).click();
  await expect
    .poll(() => answers.map((write) => write.input))
    .toEqual([{ expected_edited_at: null, selected: [], text: ANSWER }]);

  const one = [
    {
      holdsReference: true,
      sameEvent: true,
      session: PICKED.session_id,
      targets: [`session:${PICKED.session_id}`],
    },
  ];
  const message = `the copies of the answer to ${askId} the bus carried to ${PICKED.title}`;
  await expect.poll(() => answerCopies(askId, PICKED.session_id), { message }).toEqual(one);
  await expect
    .poll(() => recordedFor(PICKED.session_id, "answer", askId), {
      message: `the outbox's record of the answer's copy for ${PICKED.title}`,
    })
    .toBe(true);
  expect(
    await answerCopies(askId, PICKED.session_id),
    `${message}, read again once the outbox recorded it`
  ).toEqual(one);
}

// SLOW(>=2s): it seeds an issue, three live sessions and an ask, answers in the Inbox through the
// picker, and waits for the outbox to publish the answer and record that it did.
test("an Inbox answer that picks an agent in the answer box reaches that agent's session, once, and no session it does not name", async ({
  browser,
}) => {
  // A stack with no bus fails here, at once: the read is refused, and so is this row.
  await getBusFrames();
  const { ask } = await seedAsk();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await answerNamingThePickedAgent(page, ask.id);
  } finally {
    await alice.close();
  }
  // The asker has its copy too, by the ask's follower route, which is what makes the clause
  // below a reading of copies the answer did make rather than of a bus that carried none.
  await expect
    .poll(async () => (await answerCopies(ask.id, ASKER.session_id)).length, {
      message: `the copies of the answer the bus carried to ${ASKER.title}, who asked`,
    })
    .toBe(1);
  expectNoneOf(await answerCopies(ask.id), {
    amongAtLeast: 2,
    are: (copy) => copy.session === BYSTANDER.session_id,
    matched: `copies for ${BYSTANDER.title}, whom the answer names nowhere and who does not follow the ask`,
    measured: "copies of the answer the bus carried",
  });
});

// SLOW(>=2s): it answers through the Inbox as the row above does, then writes two replies in the
// ask's thread and waits for the outbox to publish the second and record that it did.
test("an agent an answer named replies in the ask's thread and gets the next reply", async ({
  browser,
}) => {
  await getBusFrames();
  const { ask, issue } = await seedAsk();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await answerNamingThePickedAgent(page, ask.id);
  } finally {
    await alice.close();
  }
  // The agent the answer named replies in the ask's thread as its own session, which makes it a
  // follower of the ask (`asks.QueueFollowAuthor`); its own reply reaches it on no route, since
  // the outbox leaves out the event's own actor.
  await createComment(
    issue.key,
    { ask_id: ask.id, body: "I take the rollout from Monday." },
    { actor: { id: PICKED.session_id, kind: "session" }, as: "agent" }
  );
  // Then a person replies in the thread, and the agent, now a follower, gets that reply.
  const next = await createComment(issue.key, {
    ask_id: ask.id,
    body: "Thanks: start with the canary.",
  });
  const message = `the replies in the ask's thread the bus carried to ${PICKED.title}`;
  await expect.poll(() => threadReplies(ask.id, PICKED.session_id), { message }).toEqual([next.id]);
  await expect
    .poll(() => recordedFor(PICKED.session_id, "comment", next.id), {
      message: `the outbox's record of the reply's copy for ${PICKED.title}`,
    })
    .toBe(true);
  expect(
    await threadReplies(ask.id, PICKED.session_id),
    `${message}, read again once the outbox recorded it`
  ).toEqual([next.id]);
});

for (const box of BUS_BOXES) {
  // SLOW(>=2s): each row seeds the writing list's world, walks the box's three doors, sends its
  // question back through the listener and its answer by the bus, and waits for each to land.
  test(`${box}: three doors over the same rows, the same words, a pick at the caret and its own hint`, async ({
    browser,
  }) => {
    // The budget `e2e/writing-surfaces.e2e.ts` gives every box's row, which walks the same box.
    test.setTimeout(120_000);
    await getBusFrames();
    // Over an ask `ASKER` asked rather than the world's own session, so the copy of the answer
    // that session gets is the one the answer's text names it for (`seedWritingBoxes`).
    await driveWritingBox(browser, box, { askedBy: ASKER.session_id });
  });
}
