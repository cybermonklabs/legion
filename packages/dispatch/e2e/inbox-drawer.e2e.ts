import { getAsk, getIssue, patchIssue, setAgentStreamResponder } from "./api";
import { listOf } from "./filters";
import { attachShot, drawer, seedAsk } from "./inbox-drawer";
import { priorityTrigger } from "./menus";
import {
  detailRead,
  expectReadWhileHeld,
  expectShows,
  holdIssuePatches,
  inboxRead,
  lastEventSeq,
  logReads,
  openInboxDrawer,
  pagePicker,
  pickIn,
  readAnsweredSince,
  savesEnded,
  storedValues,
  watchShown,
} from "./pick-writes";
import { expect, test } from "./test";
import { asUser } from "./users";

/**
 * The Inbox as a drawer over any page (LEGION-547), in the row whose claim reaches the server:
 * a live conversation with a half-typed message under a drawer in which an ask is answered, the
 * answer read back from the server and the draft still there afterwards. Where the drawer opens
 * from, what it covers and the key that toggles it are in `inbox-drawer.stub.e2e.ts`.
 */

test("a half-typed message in a live conversation survives opening the drawer, answering an ask in it and closing it", async ({
  browser,
}, testInfo) => {
  const { askId } = await seedAsk("Ship the drawer behind a flag?");
  const sessionID = "01a0e0c1-0000-7000-8000-0000000d4a3e";
  await setAgentStreamResponder(sessionID, true);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto(`/agents/${sessionID}/live`);
    await expect(page.getByTestId("agent-conversation")).toBeVisible();
    const composer = page.getByTestId("agent-composer").locator("textarea");
    await composer.fill("half a thought about the rollout");
    // `i` is a page key, so it never fires from inside a text field.
    await composer.blur();

    await page.keyboard.press("i");
    await expect(drawer(page)).toBeVisible();
    const card = drawer(page).getByTestId(`ask-${askId}`);
    await expect(card).toBeVisible();
    await attachShot(page, testInfo, "inbox-drawer-over-agent-conversation");

    await card.getByRole("radio", { name: "Yes" }).check();
    await card.getByRole("button", { exact: true, name: "Answer" }).click();
    await expect(card).toHaveCount(0);
    await expect
      .poll(() => getAsk(askId))
      .toMatchObject({ ask: { answer: { selected: ["Yes"], user: "alice" }, state: "answered" } });

    await page.keyboard.press("Escape");
    await expect(drawer(page)).toHaveCount(0);
    // The conversation underneath was never unmounted: same route, same draft.
    await expect(page).toHaveURL(new RegExp(`/agents/${sessionID}/live$`));
    await expect(composer).toHaveValue("half a thought about the rollout");
  } finally {
    await context.close();
  }
});

// The rows of dispatch://INFRA-299 where the Inbox drawer opens over an issue page: the drawer's
// card rows carry a priority picker of their own (`InboxDrawer`, `pane={false}`), so the issue's
// priority has a second picker beside the page's own, and the drawer lists the reader's own rows,
// so an assignee picked on the page decides whether the issue's row is listed there. The drawer
// opens through the control the issue page draws for it (`openInboxDrawer`), and is modal: the
// page under it is not pressed while it is open. Each save is held by Playwright's route control
// at least `PICK_HOLD_MS` (`holdIssuePatches`), what the server received is read from the
// issue's own events (`storedValues`), and the value stored from a fresh read once every save
// has ended.

/** A project, and an issue of it at P3 that `assignee` holds, with a question open on it that
 *  nobody has handed on (`seedAsk`), so it has a row in the Inbox of whoever holds the issue. */
async function issueWithQuestion(assignee: string): Promise<{ askId: string; key: string }> {
  const { askId, issueKey } = await seedAsk("Ship it behind a flag?");
  await patchIssue(issueKey, { assignee, priority: 3 });
  return { askId, key: issueKey };
}

// SLOW(>=2s): two saves each held three seconds, with the drawer opened over the issue page.
test("picker keeps the newest pick: a pick in the Inbox drawer's row after two in the issue page's own picker is the one the server stores last", async ({
  browser,
}) => {
  const { key } = await issueWithQuestion("alice");
  const seq = await lastEventSeq(key);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const held = await holdIssuePatches(page, key);
    const headerPicker = pagePicker(page, `Priority of ${key}`);
    await page.goto(`/issues/${key}`);
    await expectShows(headerPicker, "3");

    await pickIn(headerPicker, "1");
    await expect.poll(() => held.sent.length).toBe(1);
    // P1 is out, held: P2 waits its turn in the store's line, and P0, made in the other picker
    // of the same field, takes it out.
    await pickIn(headerPicker, "2");
    await expectShows(headerPicker, "2");
    await openInboxDrawer(page);
    const rowPicker = priorityTrigger(drawer(page), key);
    await expectShows(rowPicker, "2");
    await pickIn(rowPicker, "0");
    await expectShows(rowPicker, "0");
    expect(
      held.sent.map((patch) => patch.body),
      "P1 is still the only save out"
    ).toEqual([{ priority: 1 }]);
    held.release();
    await savesEnded(page, held);

    expect(
      await storedValues(key, "priority", seq),
      `the server received the picks in this order; the route passed on ${JSON.stringify(held.forwarded)}`
    ).toEqual([1, 0]);
    expect(await getIssue(key)).toMatchObject({ priority: 0 });
    await expectShows(headerPicker, "0");
  } finally {
    await context.close();
  }
});

// The assignee, picked on the issue page: the signed-in reader, alice, picked from another person,
// bob, on an issue whose ask nobody handed on, so the issue's row enters her Inbox drawer at the
// pick and the person it waits on follows the assignee (`useIssueAssignee`). The row stays in the
// drawer from its first paint until every save ends - an answer that took it out would be the
// older value - while a rename of the issue, made with the drawer open and the save held, has the
// issue and the Inbox read again.
// SLOW(>=2s): a save held three seconds while the issue and the Inbox are read again.
test("picker keeps a pick on screen: the assignee on the issue page and on its Inbox row, over reads that answer while its save is out", async ({
  browser,
}) => {
  const { askId, key } = await issueWithQuestion("bob");
  const renamed = "Picked under a held save, renamed while it is out";
  const inDrawer = '[role="dialog"][aria-label="Inbox"]';
  // Nobody handed the ask on: it waits on bob, the issue's assignee.
  expect((await getAsk(askId)).ask).toMatchObject({ asked_of: null, waiting_on_person: "bob" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const shownOnPage = await watchShown(page, `Assignee of ${key}`, "main");
    // The drawer lists the reader's own rows: the issue's row is listed, and draws its priority
    // picker, while the issue's question waits on her - while she is its assignee. Its card names
    // the person it waits on in its hand-on picker, which shows the assignee picked.
    const listedInDrawer = await watchShown(page, `Priority of ${key}`, inDrawer);
    const waitingInDrawer = await watchShown(page, "Hand this question to", inDrawer);
    const held = await holdIssuePatches(page, key);
    const detailReads = logReads(page, detailRead(key));
    const inboxReads = logReads(page, inboxRead);
    const picker = pagePicker(page, `Assignee of ${key}`);
    await page.goto(`/issues/${key}`);
    await expect(picker).toHaveAccessibleName(`Assignee of ${key} bob`);

    // Its people are read once the reader reaches for the control.
    await picker.focus();
    await picker.click();
    await listOf(picker).getByRole("option", { exact: true, name: "alice" }).click();
    await expect.poll(() => shownOnPage.values.at(-1)).toBe("alice");
    const fromPick = shownOnPage.values.length - 1;
    await expect.poll(() => held.sent.length).toBe(1);
    await openInboxDrawer(page);
    await expect(priorityTrigger(drawer(page), key)).toBeVisible();
    // A rename of the issue, straight to the server, with the issue's row painted in the drawer
    // and the save held: its live event has the page read the issue and its Inbox rows again, and
    // each answer, carrying bob, lands while the save is held.
    const renamedAt = Date.now();
    await patchIssue(key, { title: renamed });
    await expect
      .poll(() => [detailReads, inboxReads].every((reads) => readAnsweredSince(reads, renamedAt)))
      .toBe(true);
    await expect(priorityTrigger(drawer(page), key)).toBeVisible();
    held.release();
    await savesEnded(page, held);

    expectReadWhileHeld(detailReads, held, renamedAt);
    expectReadWhileHeld(inboxReads, held, renamedAt);

    expect(
      shownOnPage.values.slice(fromPick),
      "every assignee the page showed from the pick on"
    ).toEqual(["alice"]);
    // From the first paint of the issue's row in the drawer on, the row is listed - `(none)` is
    // the row gone - and waits on alice alone, never on bob.
    const fromListed = listedInDrawer.values.findIndex((value) => value !== "(none)");
    expect(
      listedInDrawer.values.slice(fromListed),
      `the drawer's row of the issue, from its first paint: ${JSON.stringify(listedInDrawer.values)}`
    ).toEqual(["3"]);
    const fromDrawn = waitingInDrawer.values.findIndex((value) => value !== "(none)");
    // The hand-on is the shared list, whose trigger shows `Asked of <login>`, not the login alone.
    expect(
      waitingInDrawer.values.slice(fromDrawn),
      `the person the drawer's row waits on, from its first paint: ${JSON.stringify(waitingInDrawer.values)}`
    ).toEqual(["Asked of alice"]);
    expect(await getIssue(key)).toMatchObject({ assignee: "alice", title: renamed });
    await expect(picker).toHaveAccessibleName(`Assignee of ${key} alice`);
    await expect(priorityTrigger(drawer(page), key)).toBeVisible();
  } finally {
    await context.close();
  }
});
