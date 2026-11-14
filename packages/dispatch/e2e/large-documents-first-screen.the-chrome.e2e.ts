// The chrome a document address holds back until the editor's first batch, and where it lands
// when it comes: the shell's parts and the issue page's own chrome are drawn after that batch,
// they are drawn all the same where the socket never answers, their arriving moves the
// document's first block by no pixels, and a reader who reaches for them before the batch has
// them then. None of these rows is timed: each reads what the page drew and which side of the
// first batch it fell on, never how long any of it took.
//
// These rows stood in `large-documents-first-screen.e2e.ts` until that file was split at its
// seams; they are a file of their own under a name that lists right after it, so every project
// runs them in the order it did. The reads the page holds behind the same batch, and the rest of
// what a reader is given before their editor, are in that file; the reach these rows take by
// pointer, taken by a reader with no pointer, is in
// `large-documents-first-screen.the-keyboard.e2e.ts`. What these three read the page's order by
// is `large-documents-after-the-batch.ts`. The other files of this family, and what each holds,
// are listed once, at the head of `large-documents-rows.ts`, which holds what they share.
import { expect, test } from "@playwright/test";

import { createAsk, patchIssue } from "./api";
import { documentTransport, editorBeingBuilt } from "./editor";
import { proseOnlyDocument } from "./large-documents";
import {
  readTheChrome,
  readTheReads,
  settledWithTheSocketHeldMs,
  watchTheChrome,
  watchTheReads,
} from "./large-documents-after-the-batch";
import { aDrawWithinMs, agent, aLongDrawWithinMs, seedDocument } from "./large-documents-rows";
import { withoutScrollAnchoring } from "./mermaid-helpers";
import { expectNoneOf } from "./none-of";
import { asUser } from "./users";

// Every wait of this file for something the page draws is bounded by `aDrawWithinMs` or
// `aLongDrawWithinMs` (`large-documents-rows.ts`, which says the leg these rows run in, the rule
// a bound is held to and what the waits it bounds cost).

// The reads the page holds behind the editor's first batch, in
// `large-documents-first-screen.e2e.ts`, were half the rule; the React that draws their panels
// is the other half. Measured on this pod, the shell's own render and its commits cost the main
// thread 1,062 ms on the way to the first editable block of the 28-section document, and the
// issue page's header, tab strip and artifact chrome 522 ms of React inside a 1,065 ms window.
// The route has to render; the sidebar, the inbox peek, the live pill, the needs-you and
// replies counts and the page's own chrome do not, so each of them is drawn after the editor's
// first batch (dispatch://INFRA-341 R11, `web/src/features/doc/AfterTheFirstBatch.tsx`).
//
// Each row below holds the document's socket (`documentTransport`), so the batch happens when
// the row says so rather than when a loaded machine gets round to it, and names every part it
// watches by what that part leaves in the page.

/** The parts a document address draws after the batch, by what each leaves in the page. The
 *  tab is named by its own id rather than by its role, because the margin draws tabs of its
 *  own. The shell's part differs with the width: above the phone layout it is the sidebar's
 *  navigation column, and on a phone the count the compact header's inbox entry carries,
 *  which is the same needs-you read. The entry itself stands there before the batch, at a
 *  count of nothing, so the count is what arrives (`web/src/features/shell/ShellCounts.tsx`).
 */
function theChromeAfterTheBatch(phone: boolean): Record<string, string> {
  return {
    "the artifact's header": '[data-testid="artifact-header"]',
    "the issue's header": "[data-issue-header]",
    "the issue's tab strip": "#issue-spec-tab",
    ...(phone
      ? { "the shell's inbox count": "[data-inbox-count]" }
      : { "the sidebar's navigation": 'nav[aria-label="Navigation"]' }),
  };
}

/** Puts what a chrome row read on its annotations, which the report keeps with the result, and
 *  on stdout, which `--reporter line` prints as the row runs, exactly as `printTheOrder` of
 *  `large-documents-first-screen.e2e.ts` does: a report says which part was drawn when rather
 *  than only that the row was green. */
function printTheChrome(
  what: string,
  firstBatchMs: number | undefined,
  drawn: Readonly<Record<string, number>>
): void {
  const parts = Object.entries(drawn).sort(([left], [right]) => left.localeCompare(right));
  const line = `first batch ${
    firstBatchMs === undefined ? "never" : `${Math.round(firstBatchMs)} ms`
  }; ${
    parts.length === 0
      ? "nothing of the chrome drawn"
      : parts.map(([name, at]) => `${name} ${Math.round(at)} ms`).join("; ")
  }`;
  test.info().annotations.push({ description: line, type: what });
  console.log(`${what} · ${line}`);
}

test("the shell's parts and the page's chrome are drawn after the editor's first batch @phone", async ({
  browser,
}, testInfo) => {
  test.setTimeout(300_000);
  const { issueKey, path } = await seedDocument("specs-chrome.md", proseOnlyDocument);
  // One ask of an agent's, so the shell has a count to draw: with nothing waiting on the
  // reader the compact header's peek reads `Inbox` before the batch and after it, and the row
  // would be watching for a part that never changes.
  // The count is the reader's own, which is an ask waiting on a human on an issue assigned to
  // them (`isMine` in `web/src/features/inbox/sections.ts`), so the issue is alice's here.
  await patchIssue(issueKey, { assignee: "alice" });
  await createAsk(issueKey, { question: "Which ledger settles first?" }, agent);
  const phone = testInfo.project.name === "iphone";
  const parts = theChromeAfterTheBatch(phone);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    await watchTheChrome(page, parts);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);

    // The reader has the document's own draw and the boxes the page keeps where its chrome
    // will stand; no editor is being built, so none of the chrome itself is drawn.
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeVisible({
      timeout: aDrawWithinMs,
    });
    await expect(page.getByTestId("issue-tabs-box")).toBeAttached();
    // The settled window: a first batch, or any chrome read behind it, would have gone out
    // inside it, so what the row reads next is what the held page never asked for
    // (`settledWithTheSocketHeldMs`, `large-documents-after-the-batch.ts`).
    await page.waitForTimeout(settledWithTheSocketHeldMs);

    const heldReads = await readTheReads(page);
    const heldChrome = await readTheChrome(page);
    printTheChrome("the chrome with the socket held", heldReads.firstBatchMs, heldChrome);
    expect(heldReads.firstBatchMs).toBeUndefined();
    expect(
      Object.keys(heldChrome).sort(),
      "a part of the chrome was drawn before the editor's first batch"
    ).toEqual([]);

    // Released, the editor builds, and every part is drawn behind its first batch.
    await transport.release();
    await expect(editorBeingBuilt(page)).toBeAttached({ timeout: aDrawWithinMs });
    await expect
      .poll(async () => Object.keys(await readTheChrome(page)).sort(), {
        message: "a part of the chrome the page held behind the first batch was never drawn",
        timeout: aDrawWithinMs,
      })
      .toEqual(Object.keys(parts).sort());

    const afterReads = await readTheReads(page);
    const afterChrome = await readTheChrome(page);
    printTheChrome("the chrome after the first batch", afterReads.firstBatchMs, afterChrome);
    const firstBatchMs = afterReads.firstBatchMs ?? 0;
    expect(firstBatchMs).toBeGreaterThan(0);
    for (const [name, at] of Object.entries(afterChrome)) {
      expect(at, `${name} was drawn before the editor's first batch`).toBeGreaterThan(firstBatchMs);
    }
    // And the box the tab strip stood in has gone, which is the strip itself standing there.
    await expect(page.getByTestId("issue-tabs-box")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// And the other end of that hold. A socket that opens and then neither connects nor fails
// tells the page nothing, so none of the releases above ever comes; the reader would be left
// with the document's own text and a header and tab strip that stay a skeleton for as long as
// they are on the page, which is the way out of a document that is not opening. The hold is
// bounded for that reason (`web/src/features/doc/first-batch.ts`, `LONGEST_HOLD_MS`), and this
// is the row that says so: the socket is held throughout, no batch ever comes, and the chrome
// is drawn all the same.
test("the chrome is drawn even where the socket never answers @phone", async ({
  browser,
}, testInfo) => {
  test.setTimeout(300_000);
  const { issueKey, path } = await seedDocument("specs-deadline.md", proseOnlyDocument);
  await patchIssue(issueKey, { assignee: "alice" });
  await createAsk(issueKey, { question: "Which ledger settles first?" }, agent);
  const parts = theChromeAfterTheBatch(testInfo.project.name === "iphone");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    await watchTheChrome(page, parts);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);

    // Every part of the chrome, with the socket still held and no batch behind any of them.
    await expect
      .poll(async () => Object.keys(await readTheChrome(page)).sort(), {
        message: "the chrome was never drawn on a page whose socket never answered",
        timeout: aLongDrawWithinMs,
      })
      .toEqual(Object.keys(parts).sort());
    const held = await readTheReads(page);
    printTheChrome("the chrome with the socket never answering", held.firstBatchMs, {
      ...(await readTheChrome(page)),
    });
    expect(held.firstBatchMs, "a batch came, so this row proved nothing").toBeUndefined();
    await transport.release();
  } finally {
    await alice.close();
  }
});

// The rule that chrome is held to, at the place the chrome stands over: the document's own
// first block. Every part above the document is drawn into a box the page already kept - the
// header's, the tab strip's, the artifact card's frame - and what the part turns out to be
// taller than its box by is a growth above a reader who has placed themselves, which the page
// answers by moving with it (`ChromeAfterTheFirstBatch`). So the first block stands on the
// same line of their screen before the chrome and after it.
//
// The reader here has read the chrome's boxes off the top of their screen, which is where a
// reader of a document on a phone is seconds into the load: the chrome stands 250 px down a
// 664 px screen before it is drawn and 590 px down once it is. The browser's own scroll
// anchoring is off (`withoutScrollAnchoring`), as it is in Safari before 27, so what keeps
// them in place is the page's own doing.
test("the chrome arriving moves the document's own first block by no pixels @phone", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  const { path } = await seedDocument("specs-chrome-place.md", proseOnlyDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeVisible({
      timeout: aDrawWithinMs,
    });
    await withoutScrollAnchoring(page);
    // The reader's own draw of the whole document, before they are placed in it: the page
    // stops growing under them, and the draw stops painting when the build starts
    // (`DocumentPreview`'s `building`), so a draw still painting would shorten the page under
    // the reader for a reason that is not the chrome's.
    await page.waitForTimeout(settledWithTheSocketHeldMs);

    // The reader reads the chrome's boxes off the top of their screen and stops with the
    // document's first block just under it. Every box of the chrome now stands above them,
    // which the row proves before it measures anything: a box a reader can see is a box they
    // watch land, and nothing moves for them (`readersBlock`).
    const landed = await page.evaluate(() => {
      const block = document.querySelector('[aria-label="Document text"] [data-block-id]');
      if (block === null) return undefined;
      window.scrollBy(0, block.getBoundingClientRect().top - 16);
      return Math.round(block.getBoundingClientRect().top);
    });
    expect(landed, "the row never found the document's first block").toBe(16);
    // Over a chrome that was really drawn rather than over nothing: "every box is above the
    // reader" is met by a renamed test id or a chrome not yet mounted, so the clause says
    // nothing until the boxes it is about are in the page - the tab strip's box and the issue
    // header, which is the skeleton's own `header` while the socket is held
    // (dispatch://INFRA-341, the pairing every universal clause of a check takes, as the row
    // above does at its own read).
    //
    // The strip's box is named before the count and not left to it: this row is `@phone`, where
    // the shell draws a `header` of its own beside the issue's (`web/src/app.tsx`), so two boxes
    // are in the page whether or not `issue-tabs-box` is one of them and the floor below is met
    // by a strip that was renamed or never mounted.
    await expect(page.getByTestId("issue-tabs-box")).toBeAttached();
    const chrome = await page.evaluate(() =>
      [...document.querySelectorAll('[data-testid="issue-tabs-box"], header')].map((box) => ({
        name: box.getAttribute("data-testid") ?? box.tagName.toLowerCase(),
        top: Math.round(box.getBoundingClientRect().top),
      }))
    );
    expectNoneOf(chrome, {
      amongAtLeast: 2,
      are: (box) => box.top >= 0,
      matched: "boxes the chrome stands in that were still on the reader's screen",
      measured: "boxes of the chrome above the document",
    });

    // From here the row reads the first block after every frame the browser paints - a task
    // scheduled from an animation frame runs once that frame is on the screen - so what it
    // records is what the reader saw. The block is looked up afresh each time: the editor's
    // first batch puts its own copy of it in front of the draw's, and both stand at the top of
    // the document surface, which is what the chrome above moves.
    await page.evaluate(() => {
      const first = () =>
        document.querySelector(
          '.proof-editor [data-block-id], [aria-label="Document text"] [data-block-id]'
        );
      const block = first();
      window.__firstBlockPlace =
        block === null
          ? undefined
          : { grew: false, moved: 0, started: block.getBoundingClientRect().top };
      const scrolled = document.scrollingElement?.scrollTop ?? 0;
      const sample = () => {
        const place = window.__firstBlockPlace;
        const now = first();
        if (place === undefined || now === null) return;
        const top = now.getBoundingClientRect().top;
        const above = top + (document.scrollingElement?.scrollTop ?? 0) - scrolled;
        place.moved = Math.max(place.moved, Math.abs(top - place.started));
        if (Math.abs(above - place.started) > 0.5) place.grew = true;
        requestAnimationFrame(() => window.setTimeout(sample, 0));
      };
      sample();
    });

    await transport.release();
    await expect(page.locator("[data-issue-header]")).toBeAttached({ timeout: aDrawWithinMs });
    await expect(page.locator('[data-testid="artifact-header"]')).toBeAttached({
      timeout: aDrawWithinMs,
    });
    await expect
      .poll(async () => (await page.evaluate(() => window.__firstBlockPlace))?.grew, {
        message: "the chrome landing never changed how much page stands above the reader",
        timeout: aDrawWithinMs,
      })
      .toBe(true);

    const place = await page.evaluate(() => window.__firstBlockPlace);
    test.info().annotations.push({
      description: `the first block started at ${Math.round(place?.started ?? Number.NaN)} px and moved ${place?.moved ?? Number.NaN} px`,
      type: "the chrome moves the document by no pixels",
    });
    console.log(
      `the chrome moves the document by no pixels · started ${Math.round(place?.started ?? Number.NaN)} px, moved ${place?.moved ?? Number.NaN} px`
    );
    // Not to the exact float: a scroll offset is a fraction of a pixel, so the correction lands
    // within one, and a pixel is the unit a reader has.
    expect(
      place?.moved ?? Number.NaN,
      "the chrome moved the document's own first block"
    ).toBeLessThan(1);
  } finally {
    await alice.close();
  }
});

// And the other half of the rule, for the chrome as for the reads: a part is held from a
// reader who is not waiting for it, never from one who is. The reader here presses the box the
// tab strip will stand in - which is how a reader reaches for a tab - with the document's
// socket still held, so no first batch has happened and none is on its way.
test("a reader who reaches for the chrome before the batch has it then @phone", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  const { path } = await seedDocument("specs-chrome-reached.md", proseOnlyDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    await documentTransport(page, { holding: true });
    await page.goto(path);

    const box = page.getByTestId("issue-tabs-box");
    await expect(box).toBeAttached({ timeout: aDrawWithinMs });
    // The settled window: with the socket held no batch ever comes, and this is how long the
    // row gives it before it reads that none did (`settledWithTheSocketHeldMs`).
    await page.waitForTimeout(settledWithTheSocketHeldMs);
    expect((await readTheReads(page)).firstBatchMs).toBeUndefined();
    await expect(page.getByRole("tab", { name: /^Spec$/ })).toHaveCount(0);

    await box.click({ position: { x: 5, y: 5 } });

    await expect(page.getByRole("tab", { name: /^Spec$/ })).toBeVisible({ timeout: 30_000 });
    // And it was the reader's own press that did it, with no first batch behind it.
    expect((await readTheReads(page)).firstBatchMs).toBeUndefined();
  } finally {
    await alice.close();
  }
});
