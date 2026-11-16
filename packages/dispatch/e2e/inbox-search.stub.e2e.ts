/** The Inbox's search box (INFRA-963): one field beside the assignee filter that keeps the rows
 *  holding every word a reader types and drops the rest, matching an ask's issue key, its issue
 *  title, the name of the document it stands on where it has no issue, its question and its
 *  author, whatever the case.
 *
 *  Every row here reads what the page draws over one answer - which rows are left, what the
 *  counts say, where the focus is and what the address holds - so each runs against a recording
 *  rather than against a harness (packages/dispatch/AGENTS.md, "The stubbed layer"). The one row
 *  of the family that holds a write open and lets it through stayed in `inbox-search.e2e.ts`.
 *
 *  Two seeds: the file's own eight questions, and the three the last row needs, which must have
 *  the kept question standing above both rows the text leaves.
 */
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { expect } from "@playwright/test";

import { createAsk, createIssue, createProject } from "./api";
import {
  closeAssigneeFilter,
  inboxSearchBox,
  openAssigneeFilter,
  pickAssignee,
  searchInbox,
} from "./inbox-filter";
import { inboxRow, paneScroller, selectedRow, selectRow } from "./inbox-pane";
import {
  AUDITOR,
  emptyState,
  INBOX,
  openInbox,
  PLANNER,
  rows,
  SILENT_SESSION,
  seedInbox,
} from "./inbox-search";
import { stubbed } from "./stub/stub-test";

const test = stubbed(seedInbox);

test.describe("inbox search", () => {
  // The box is the Inbox's own control and the keyboard's: `f` reaches it from anywhere on the
  // page, and the assignee popover's own field - a `combobox` named "Search people" - is a
  // different control, so a reader (and a screen reader) has exactly one "Search asks".
  test("the header holds one Search asks box, which `f` puts the focus in @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.listed);

    const box = inboxSearchBox(page);
    await expect(box).toHaveCount(1);
    await expect(box).toHaveValue("");
    await expect(box).toHaveAttribute("placeholder", "Issue, title, question or author");
    // The cap is said rather than enforced by the field, so the change handler's cut is the
    // one rule (proved by the address row below).
    expect(await box.getAttribute("maxlength")).toBeNull();
    await expect(box).toHaveAttribute("title", `At most ${SEARCH_QUERY_MAX} characters`);

    // With the assignee popover open both fields are on the page, and they are not the same
    // control: only one of them is the Inbox's searchbox.
    await openAssigneeFilter(page);
    await expect(page.getByRole("combobox", { name: "Search people" })).toBeVisible();
    await expect(page.getByRole("searchbox", { name: "Search asks" })).toHaveCount(1);
    await closeAssigneeFilter(page);

    // `f` from the page itself, with the focus on nothing.
    await page.locator("body").focus();
    await page.keyboard.press("f");
    await expect(box).toBeFocused();
    await expect(box).toHaveValue("");

    // And `f` inside the box is a letter, not the key: the binding is not `inEditable` and a
    // `search` input takes typed text (`keymap.ts`'s `TEXT_INPUT_TYPES`), so the registry
    // passes the key through to the field rather than re-focusing what the reader is already
    // in. The word is the seed's own, so the narrowing it makes is real too.
    await page.keyboard.type("drift");
    await expect(box).toHaveValue("drift");
    await expect(box).toBeFocused();
    await expect(rows(page)).toHaveCount(2);
  });

  // One narrowing per field the rule reads, each one two words: the row kept holds both, and the
  // rows holding one of them go. The second word is always carried by a field other than the one
  // under test, so a row that survives proves that field was read.
  test("two words keep the rows holding both, matched on the key, the title, the question and the author @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.listed);

    // The issue key, which carries the project key: `ledger` is in no title, question or
    // author, so the four LEDGER rows hold it through their keys alone.
    await searchInbox(page, "ledger");
    await expect(rows(page)).toHaveCount(4);
    await searchInbox(page, "ledger lease");
    await expect(rows(page)).toHaveCount(1);
    await expect(inboxRow(page, scene.retryAskId)).toBeVisible();

    // The same two words in any case: the rule lowercases both sides.
    await searchInbox(page, "LEDGER Lease");
    await expect(rows(page)).toHaveCount(1);
    await expect(inboxRow(page, scene.retryAskId)).toBeVisible();

    // The issue title: `clock` and `drift` are both in "Clock drift" and nowhere else in that
    // row, and a second row holds `drift` in its question alone.
    await searchInbox(page, "drift");
    await expect(rows(page)).toHaveCount(2);
    await searchInbox(page, "clock drift");
    await expect(rows(page)).toHaveCount(1);
    await expect(inboxRow(page, scene.clockAskId)).toBeVisible();

    // The question: `cold` is in two questions, `pass` in one of them.
    await searchInbox(page, "cold");
    await expect(rows(page)).toHaveCount(2);
    await searchInbox(page, "cold pass");
    await expect(rows(page)).toHaveCount(1);
    await expect(inboxRow(page, scene.replayAskId)).toBeVisible();

    // The author, as the ask carries it: the title its origin recorded.
    await searchInbox(page, "nightly planner");
    await expect(rows(page)).toHaveCount(5);
    for (const askId of [
      scene.clockAskId,
      scene.budgetAskId,
      scene.replayAskId,
      scene.windowAskId,
      scene.runbookAskId,
    ]) {
      await expect(inboxRow(page, askId)).toBeVisible();
    }

    // The author's session id, which no title repeats whole.
    await searchInbox(page, "e2e-morning-auditor");
    await expect(rows(page)).toHaveCount(2);
    await expect(inboxRow(page, scene.retryAskId)).toBeVisible();
    await expect(inboxRow(page, scene.wakeAskId)).toBeVisible();

    // The author's owner, a login: `bob` is in no key, title or question, and `lease` is in
    // two rows, so the row kept is the one whose agent writes for Bob.
    await searchInbox(page, "lease");
    await expect(rows(page)).toHaveCount(2);
    await searchInbox(page, "bob lease");
    await expect(rows(page)).toHaveCount(1);
    await expect(inboxRow(page, scene.leaseAskId)).toBeVisible();
  });

  // The hold that keeps a row under the reader's hand while the server moves the list is not
  // consulted for the reader's own typing: a row the words exclude goes at once, whether they
  // last clicked it or are resting the pointer on it.
  test("the row last clicked and the row under the pointer drop with the rest @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.listed);

    // Clicked: the reading column is showing it, and the focus is on it.
    await selectRow(page, scene.clockAskId);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", scene.clockAskId);

    // Pointed at: the pointer rests in the middle of another row and stays there.
    const pointed = inboxRow(page, scene.leaseAskId);
    const box = await pointed.boundingBox();
    if (box === null) {
      throw new Error("the Inbox drew no box for the row the pointer was to rest on");
    }
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await expect(pointed).toBeVisible();

    // Neither survives words it does not hold.
    await searchInbox(page, "cold pass");
    await expect(rows(page)).toHaveCount(1);
    await expect(inboxRow(page, scene.clockAskId)).toHaveCount(0);
    await expect(inboxRow(page, scene.leaseAskId)).toHaveCount(0);
    await expect(inboxRow(page, scene.replayAskId)).toBeVisible();
  });

  // The text narrows where the assignee filter's narrowing is applied, so the two compose
  // whichever way the list was already filtered; what sits above the bands is untouched.
  test("the text narrows inside the assignee filter, ?agent= and ?section=needs-you, and leaves the sections above the bands alone @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.listed);

    // Above the bands: the credential request and the summary of what waits on the reader -
    // her four questions and the one request - which the typed text never moves.
    const credentials = page.getByRole("region", { name: "Credential requests" });
    const blocked = page.getByRole("link", { name: /^Blocked on you: 5 items/ });
    await expect(credentials.getByRole("link")).toHaveCount(1);
    await expect(credentials.getByRole("link")).toContainText("LEDGER_API_KEY");
    await expect(blocked).toBeVisible();

    // Inside the filter's own selection: Bob holds one issue, with two questions on it.
    await pickAssignee(page, "bob");
    await closeAssigneeFilter(page);
    await expect(rows(page)).toHaveCount(2);
    await searchInbox(page, "window first");
    await expect(rows(page)).toHaveCount(1);
    await expect(inboxRow(page, scene.windowAskId)).toBeVisible();
    await expect(credentials.getByRole("link")).toHaveCount(1);
    await expect(blocked).toBeVisible();

    // Inside `?agent=`: that agent's two questions, then the one of them holding the word.
    await page.goto(`/?agent=${AUDITOR.actor.id}&assignee=everyone`);
    await expect(rows(page)).toHaveCount(2);
    await searchInbox(page, "lease");
    await expect(rows(page)).toHaveCount(1);
    await expect(inboxRow(page, scene.retryAskId)).toBeVisible();

    // Inside `?section=needs-you`: the row waiting on its agent is already gone, so the word
    // `cold`, which two rows hold, keeps only the other one.
    await page.goto("/?section=needs-you&assignee=everyone");
    await expect(rows(page)).toHaveCount(7);
    await expect(inboxRow(page, scene.replayAskId)).toHaveCount(0);
    await searchInbox(page, "cold");
    await expect(rows(page)).toHaveCount(1);
    await expect(inboxRow(page, scene.leaseAskId)).toBeVisible();
    await expect(credentials.getByRole("link")).toHaveCount(1);
  });

  // The counts a reader reads off the list are the narrowed list's: the band's own heading count
  // and the `ask N of M` each row's controls are named by (`features/inbox/ask-name.ts`).
  // INFRA-450 took the owner groups out of the list, so the count that stands over a set of rows
  // is the View menu's own band; **Waiting on** is the banding the sections below stand for.
  test("the band counts and the row ordinals count the narrowed list @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.listed);
    await page.goto("/?group=waiting-on");

    // What the band's own heading says, which is what a reader counts the band by.
    const bandCount = async (band: string): Promise<number> => {
      const heading = await page
        .getByRole("button", { name: new RegExp(`^${band} \\(\\d+\\)$`) })
        .textContent();
      return Number(/\((\d+)\)/.exec(heading ?? "")?.[1]);
    };
    const waitingOnYou = page.locator('[data-inbox-section="human"]');

    // Three questions on one issue: three rows that say which of three.
    await expect(page.getByRole("checkbox", { name: /^Select LEDGER-2, ask 1 of 3:/ })).toHaveCount(
      1
    );
    expect(await bandCount("Waiting on you")).toBe(await waitingOnYou.count());
    // What the list draws before any word narrows it, which clearing the box must bring back.
    const drawn = await rows(page).count();
    for (const index of [2, 3]) {
      await expect(
        page.getByRole("checkbox", { name: new RegExp(`^Select LEDGER-2, ask ${index} of 3:`) })
      ).toHaveCount(1);
    }

    // The word drops one of the three; the heading and the names count what is left.
    await searchInbox(page, "ledger auditor");
    await expect(rows(page)).toHaveCount(2);
    await expect(waitingOnYou).toHaveCount(2);
    expect(await bandCount("Waiting on you")).toBe(2);
    for (const index of [1, 2]) {
      await expect(
        page.getByRole("checkbox", { name: new RegExp(`^Select LEDGER-2, ask ${index} of 2:`) })
      ).toHaveCount(1);
    }
    await expect(page.getByRole("checkbox", { name: /^Select LEDGER-2, ask 3 of 3:/ })).toHaveCount(
      0
    );

    // Cleared, every count is the whole list's again. The number is the list's own rather than
    // the seed's: INFRA-450 bands the list by the View menu's own field, and a band below the
    // first is folded, so fewer rows are drawn than the eight the seed holds and what the
    // clearing must restore is what was drawn.
    await searchInbox(page, "");
    await expect(rows(page)).toHaveCount(drawn);
    expect(await bandCount("Waiting on you")).toBe(await waitingOnYou.count());
  });

  // A mark is the reader's, and the box is a view of the list: a row the box hides leaves the
  // bar's count, because the bar never counts a row that is not on the page, and the mark itself
  // comes back with the row.
  test("a marked row the box hides leaves the bulk count and comes back marked @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.listed);

    await selectRow(page, scene.clockAskId);
    await page.keyboard.press("x");
    // INFRA-450's bar serves both kinds of row, so its own name is **Selected rows**.
    const bar = page.getByRole("group", { name: "Selected rows" });
    await expect(bar).toContainText("1 selected");
    await expect(inboxRow(page, scene.clockAskId).getByRole("checkbox")).toBeChecked();

    await searchInbox(page, "cold pass");
    await expect(inboxRow(page, scene.clockAskId)).toHaveCount(0);
    await expect(bar).toHaveCount(0);

    await searchInbox(page, "");
    await expect(rows(page)).toHaveCount(8);
    await expect(inboxRow(page, scene.clockAskId).getByRole("checkbox")).toBeChecked();
    await expect(bar).toContainText("1 selected");
  });

  // A reader who narrowed the list is told why it is empty before anything else is: ahead of the
  // agent filter's own line, and ahead of the rule that leaves the list silent while a credential
  // request waits on them.
  test("text matching no row says so ahead of every other empty line @pr", async ({
    page,
    scene,
  }) => {
    // A filter nobody matches, with a request pending: the list says nothing at all, because
    // the request above it waits on the reader as much as a question does.
    await page.goto("/?assignee=carol");
    await expect(page.getByRole("region", { name: "Credential requests" })).toBeVisible();
    await expect(rows(page)).toHaveCount(0);
    await expect(emptyState(page)).toHaveCount(0);

    // The same address with a word nothing holds: now it says why.
    await page.goto("/?assignee=carol&q=zzzq");
    await expect(rows(page)).toHaveCount(0);
    await expect(emptyState(page)).toHaveText("Nothing matches this text");
    await expect(page.getByRole("region", { name: "Credential requests" })).toBeVisible();

    // An agent filter nothing matches names that agent.
    await page.goto(`/?agent=${SILENT_SESSION}&assignee=everyone`);
    await expect(rows(page)).toHaveCount(0);
    await expect(emptyState(page)).toContainText("No open asks from");

    // With a word nothing holds, the text's line comes first.
    await page.goto(`/?agent=${SILENT_SESSION}&assignee=everyone&q=zzzq`);
    await expect(rows(page)).toHaveCount(0);
    await expect(emptyState(page)).toHaveText("Nothing matches this text");

    // And a word nothing holds on the plain list says the same thing, typed rather than linked.
    await openInbox(page, scene.listed);
    await searchInbox(page, "zzzq");
    await expect(rows(page)).toHaveCount(0);
    await expect(emptyState(page)).toHaveText("Nothing matches this text");
  });

  // A reader types one letter at a time, and the letter that empties the list must not take the
  // box out from under them. The empty list draws the header in a tree of its own
  // (`Inbox.tsx`'s `empty`, a plain div, against the list's `ViewportAnchor`), so React mounts a
  // fresh input there: the focus goes to the body, and the rest of the word, the Backspace that
  // would correct it and every later keystroke reach the page's own keys instead of the box.
  test("the box keeps the focus through a word that matches no row @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.listed);
    const box = inboxSearchBox(page);
    await page.keyboard.press("f");
    await expect(box).toBeFocused();

    await page.keyboard.type("ledger");
    await expect(rows(page)).toHaveCount(4);

    // The letter that empties the list.
    await page.keyboard.type("z");
    await expect(rows(page)).toHaveCount(0);
    await expect(emptyState(page)).toHaveText("Nothing matches this text");
    await expect(box).toBeFocused();

    // The rest of the word, and the correction, still reach the box.
    await page.keyboard.type("q");
    await expect(box).toHaveValue("ledgerzq");
    await page.keyboard.press("Backspace");
    await page.keyboard.press("Backspace");
    await expect(box).toHaveValue("ledger");
    await expect(rows(page)).toHaveCount(4);
  });

  // A question on a project document belongs to that document, so the document's name is what a
  // reader has to search it by - and the document's name is read only where there is no issue, so
  // `mirror` from the two MIRROR keys does not drag those rows in beside it.
  test("a question on a project document is found by a word of the document's name @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.listed);

    await searchInbox(page, "mirror");
    await expect(rows(page)).toHaveCount(4);

    await searchInbox(page, "runbook");
    await expect(rows(page)).toHaveCount(1);
    await expect(inboxRow(page, scene.runbookAskId)).toBeVisible();

    await searchInbox(page, "mirror runbook");
    await expect(rows(page)).toHaveCount(1);
    const row = inboxRow(page, scene.runbookAskId);
    await expect(row).toBeVisible();
    await expect(row.getByRole("link", { name: "Mirror runbook" })).toBeVisible();
  });

  // The address is the whole memory of the typed text: a reader can link it, reload it and walk
  // the list without losing it, and what the page writes there is cut to what a URL may carry.
  test("the typed text rides in the address, cut to the cap, and survives a reload and `j` @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.listed);

    await searchInbox(page, "ledger lease");
    await expect(rows(page)).toHaveCount(1);
    await expect.poll(() => new URL(page.url()).searchParams.get("q")).toBe("ledger lease");

    // One character over the cap: the change handler cuts what it writes, and the field shows
    // the cut text, so what is filtered by and what is in the address are the one string.
    await inboxSearchBox(page).fill("x".repeat(SEARCH_QUERY_MAX + 1));
    await expect
      .poll(() => new URL(page.url()).searchParams.get("q")?.length)
      .toBe(SEARCH_QUERY_MAX);
    await expect(inboxSearchBox(page)).toHaveValue("x".repeat(SEARCH_QUERY_MAX));
    await expect(emptyState(page)).toHaveText("Nothing matches this text");

    // A reload opens on the same narrowed list, with the same text in the box.
    await searchInbox(page, "ledger");
    await expect(rows(page)).toHaveCount(4);
    await page.reload();
    await expect(rows(page)).toHaveCount(4);
    await expect(inboxSearchBox(page)).toHaveValue("ledger");
    await expect(inboxRow(page, scene.clockAskId)).toBeVisible();

    // Walking the list rewrites the address for its selection and keeps the text in it. The
    // walk starts on the narrowed list's own first row, `LEDGER-1` being the one the issue
    // priorities put at the top of it, so there is a row below to step onto.
    await selectRow(page, scene.clockAskId);
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", scene.clockAskId);
    await page.keyboard.press("j");
    await expect(selectedRow(page)).not.toHaveAttribute("data-inbox-row", scene.clockAskId);
    await expect(rows(page)).toHaveCount(4);
    await expect(inboxSearchBox(page)).toHaveValue("ledger");
    expect(new URL(page.url()).searchParams.get("q")).toBe("ledger");
  });

  // Clearing the agent filter clears that filter alone: the reader's typed text is theirs, and
  // the list they get back is the whole inbox narrowed by it.
  test("Clear agent filter keeps the typed text and the list it narrows @pr", async ({
    page,
    scene,
  }) => {
    await page.goto(`/?agent=${AUDITOR.actor.id}&q=lease&assignee=everyone`);
    await expect(rows(page)).toHaveCount(1);
    await expect(inboxRow(page, scene.retryAskId)).toBeVisible();
    await expect(inboxSearchBox(page)).toHaveValue("lease");

    const clear = page.getByRole("link", { name: "Clear agent filter" });
    await expect(clear).toHaveText("Asks from Morning auditor · clear");
    await clear.click();

    await expect(page.getByRole("link", { name: "Clear agent filter" })).toHaveCount(0);
    await expect(inboxSearchBox(page)).toHaveValue("lease");
    await expect(rows(page)).toHaveCount(2);
    await expect(inboxRow(page, scene.retryAskId)).toBeVisible();
    await expect(inboxRow(page, scene.leaseAskId)).toBeVisible();
    const address = new URL(page.url()).searchParams;
    expect(address.get("q")).toBe("lease");
    expect(address.get("agent")).toBeNull();
  });

  // Escape is one level out, and out of the box is the list: the row the reading column is
  // showing. Clearing the box is the other way out - every row comes back.
  test("Escape leaves the box for the selected row, and clearing it restores every row @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.listed);

    await selectRow(page, scene.budgetAskId);
    await page.locator("body").focus();
    await page.keyboard.press("f");
    await expect(inboxSearchBox(page)).toBeFocused();
    await searchInbox(page, "ledger auditor");
    await expect(rows(page)).toHaveCount(2);

    // The selected row moved with the narrowing; Escape puts the focus on whichever row the
    // column is showing now.
    await inboxSearchBox(page).focus();
    await page.keyboard.press("Escape");
    const selectedId = await selectedRow(page).getAttribute("data-inbox-row");
    await expect(inboxRow(page, selectedId ?? "")).toBeFocused();
    await expect(inboxSearchBox(page)).toHaveValue("ledger auditor");

    await searchInbox(page, "");
    await expect(rows(page)).toHaveCount(8);
    for (const askId of [scene.clockAskId, scene.leaseAskId, scene.runbookAskId]) {
      await expect(inboxRow(page, askId)).toBeVisible();
    }
  });

  // The column keeps a question whose row the typed text hides while the reader has an unsent
  // answer in it (`pane-selection.ts`'s `paneAsk`), so there is a column to read with no
  // selected row beside it. Escape out of such a column is still one level out: it returns to
  // the row `l` was pressed on, rather than matching no binding and doing nothing.
  test("Escape leaves a column held open over a hidden row for the row `l` was pressed on @pr", async ({
    page,
    scene,
  }) => {
    await openInbox(page, scene.listed);

    // The reader is part way through an answer to the clock question: that unsent text is what
    // keeps the question in the column once its row goes. The column is where this width
    // draws the card, the short rows beside it carrying none, so the one answer box on the
    // page is the column's.
    const typed = "The ledger, once the lease settles.";
    await selectRow(page, scene.clockAskId);
    const answer = page.locator("[data-ask-answer]");
    await expect(answer).toHaveCount(1);
    await answer.fill(typed);

    // Two words the clock row does not hold: its row goes, its question stays in the column,
    // and the list has no selected row left to return to.
    await inboxSearchBox(page).focus();
    await searchInbox(page, "ledger auditor");
    await expect(rows(page)).toHaveCount(2);
    await expect(inboxRow(page, scene.clockAskId)).toHaveCount(0);
    await expect(selectedRow(page)).toHaveCount(0);
    await expect(answer).toHaveValue(typed);

    // Escape out of the box lands on the list's first row, and `l` from there records it. The
    // fill left the focus in the box, so the key needs no focus call of its own.
    await page.keyboard.press("Escape");
    const landedId = await rows(page).first().getAttribute("data-inbox-row");
    const landed = inboxRow(page, landedId ?? "");
    await expect(landed).toBeFocused();
    await page.keyboard.press("l");
    await expect(paneScroller(page)).toBeFocused();

    // And Escape hands the focus back to that row, with the answer still waiting in the
    // column above it. The list still holds no selected row at this moment, which is what
    // makes the recorded row the only way out: a column whose selection had come back would
    // prove nothing about the held one.
    await expect(selectedRow(page)).toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(landed).toBeFocused();
    await expect(answer).toHaveValue(typed);
  });
});

/**
 * Its own three rows, not the file's eight: the last row needs the kept question to stand first
 * with the rows the text leaves below it, which the grouped seed above does not give.
 *
 * The list heads its band with the newest ask, so the question the column will keep is written
 * last: it stands above both rows the text leaves, which is what gives `k` nowhere to step.
 */
const stepped = stubbed(async () => {
  await createProject({ key: "STEP", name: "Step service" });
  const replayIssue = await createIssue({ project: "STEP", title: "Cold replay" });
  await createAsk(replayIssue.key, { question: "Does a cold start need a second pass?" }, PLANNER);
  const leaseIssue = await createIssue({ project: "STEP", title: "Cold lease handover" });
  await createAsk(leaseIssue.key, { question: "Who holds the cold lease?" }, PLANNER);
  const topIssue = await createIssue({ project: "STEP", title: "Ledger clock drift" });
  const top = await createAsk(
    topIssue.key,
    { question: "Which source of truth does the ledger read at startup?" },
    PLANNER
  );
  return { top: top.id };
});

stepped.describe("inbox search", () => {
  // A step taken inside a column held open over a hidden row can move nothing at all: the
  // question the column keeps stood above every row the text left, so `k` has no row to step
  // to and none to take the focus. The reader is still in the column, and the row `l` recorded
  // is the only way out it has, so a key that did nothing visible must not take that away:
  // Escape after it is still one level out, to the row `l` was pressed on.
  stepped(
    "a step that moves nothing inside a held column leaves Escape its way back @pr",
    async ({ page, scene }) => {
      await page.goto(INBOX);
      await expect(rows(page)).toHaveCount(3);
      await expect(rows(page).first()).toHaveAttribute("data-inbox-row", scene.top);

      // The unsent answer is what keeps the question in the column once the text hides its row.
      const typed = "The ledger, once the lease settles.";
      await selectRow(page, scene.top);
      const answer = page.locator("[data-ask-answer]");
      await expect(answer).toHaveCount(1);
      await answer.fill(typed);

      await searchInbox(page, "cold");
      await expect(rows(page)).toHaveCount(2);
      await expect(inboxRow(page, scene.top)).toHaveCount(0);
      await expect(selectedRow(page)).toHaveCount(0);

      // Escape out of the box lands on the list's first row, and `l` from there records it.
      await page.keyboard.press("Escape");
      const landedId = await rows(page).first().getAttribute("data-inbox-row");
      const landed = inboxRow(page, landedId ?? "");
      await expect(landed).toBeFocused();
      await page.keyboard.press("l");
      await expect(paneScroller(page)).toBeFocused();

      // The step moves neither the selection nor the focus.
      await page.keyboard.press("k");
      await expect(paneScroller(page)).toBeFocused();
      await expect(selectedRow(page)).toHaveCount(0);

      await page.keyboard.press("Escape");
      await expect(landed).toBeFocused();
      await expect(answer).toHaveValue(typed);
    }
  );
});

test.describe("inbox search without a reading column", () => {
  // Below 1280 px the Inbox is the list of cards it has always been, and the box is the same
  // control doing the same thing to it: the header wraps, the cards narrow, and nothing else
  // about the narrowing changes with the width.
  test("the box narrows the card list at 1024 px and at 390 px @pr", async ({ page, scene }) => {
    for (const size of [
      { height: 900, width: 1024 },
      { height: 844, width: 390 },
    ]) {
      await page.setViewportSize(size);
      await page.goto(INBOX);
      await expect(rows(page)).toHaveCount(8);
      await expect(page.getByTestId("inbox-pane")).toHaveCount(0);

      await searchInbox(page, "ledger lease");
      await expect(rows(page)).toHaveCount(1);
      await expect(inboxRow(page, scene.retryAskId)).toBeVisible();
      // The row is the whole card here, so what the narrowing keeps is the question itself.
      await expect(
        page.getByTestId(`ask-${scene.retryAskId}`).getByText("How many attempts before the lease")
      ).toBeVisible();
      expect(new URL(page.url()).searchParams.get("q")).toBe("ledger lease");

      await searchInbox(page, "zzzq");
      await expect(emptyState(page)).toHaveText("Nothing matches this text");

      await searchInbox(page, "");
      await expect(rows(page)).toHaveCount(8);
    }
  });

  // The drawer is a peek over another page, so its text is its own state and never that page's
  // address; it draws cards at every width, so there is no reading pane for `l` to move into and
  // Escape out of the box lands on the list's first row, the drawer having no selection.
  test("the drawer's box narrows its own list, writes no address, and leaves `l` idle @pr", async ({
    page,
    scene,
  }) => {
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("i");

    // The drawer opens on the reader's own work: the four questions on Alice's two issues.
    const drawer = page.getByRole("dialog", { name: "Inbox" });
    await expect(drawer).toBeVisible();
    const drawerRows = drawer.locator("[data-inbox-row]");
    await expect(drawerRows).toHaveCount(4);
    await expect(drawer.getByTestId("inbox-pane")).toHaveCount(0);
    await expect(drawer.locator("[data-inbox-selected]")).toHaveCount(0);

    // The box in the drawer narrows the drawer's list and writes nothing to the address.
    const box = drawer.getByRole("searchbox", { name: "Search asks" });
    await box.fill("ledger auditor");
    await expect(drawerRows).toHaveCount(2);
    await expect(drawer.locator(`[data-inbox-row="${scene.retryAskId}"]`)).toBeVisible();
    await expect(drawer.locator(`[data-inbox-row="${scene.wakeAskId}"]`)).toBeVisible();
    await expect(page).toHaveURL(/\/agents$/);
    expect(new URL(page.url()).searchParams.get("q")).toBeNull();

    // `l` has no reading pane to move into here, so it moves nothing and closes nothing.
    await page.keyboard.press("Escape");
    await expect(drawerRows.first()).toBeFocused();
    await page.keyboard.press("j");
    const walked = drawerRows.nth(1);
    await expect(walked).toBeFocused();
    await page.keyboard.press("l");
    await expect(walked).toBeFocused();
    await expect(drawer).toBeVisible();

    // Escape with the focus in the box leaves the drawer open and lands on its first row.
    await box.focus();
    await expect(box).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(drawer).toBeVisible();
    await expect(drawerRows.first()).toBeFocused();
    await expect(box).toHaveValue("ledger auditor");

    // Cleared, the drawer's own list is whole again, and the page under it never moved.
    await box.fill("");
    await expect(drawerRows).toHaveCount(4);
    await expect(page).toHaveURL(/\/agents$/);
  });
});
