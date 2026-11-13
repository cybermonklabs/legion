// What a build of a large document serves a reader who answers a decision while it runs, read
// from the wall clock on the phone the spec names. The row seeds the 406,000-character document
// of `large-documents.ts` at 4x CPU throttling, because it needs a build still running when the
// page presses the card.
//
// It seeds that document with its first decision block moved from the twenty-eighth section to
// the second (`largeDocumentDecidedEarly`): the same 140 sections, the same 406,000 characters
// and the same build, with the card the reader answers inside the blocks the server draws into
// the page it sends. Why the row needs it there is in its own comment.
//
// This row stood in `large-documents-build-serves.e2e.ts`, beside the timer row, until that
// file passed 1,000 lines. It is a file of its own under a name that lists right after it, and
// like it this file is `iphone-alone`'s and no other project's: that project names both in its
// `testMatch` and every parallel project ignores both (`clockSpecNames`,
// e2e/configurations.ts), so the leg runs the two rows one at a time in the order it did. What
// the eleven `large-documents-*` files share is `large-documents-rows.ts`, which holds the probe
// this row reads a build through (`whatTheBuildServes`).
import { expect, type Page, test } from "@playwright/test";

import { listIssueAsks } from "./api";
import { documentTransport } from "./editor";
import { decisionBlockId, documentNames, largeDocumentDecidedEarly } from "./large-documents";
import { throttleTheCpu, watchTheBuildsWindow } from "./large-documents-page";
import { liveEditorRoot, namedEditor, seedDocument, topLevelBlocks } from "./large-documents-rows";
import { asUser } from "./users";

/** The page's own draw of the document's text, which is what a reader has in front of them for
 *  every block the editor's batches have not reached yet (`web/src/features/doc/DocumentPreview.tsx`),
 *  as a selector an init script can carry. */
const theDraw = '[aria-label="Document text"]';

/** The document's blocks in the editor a reader already has while the rest is built. The
 *  decision row holds it to none before the release and to some after the first batch, so a
 *  selector that matched nothing could not pass the first for it. */
const liveEditorBlocks = `${liveEditorRoot} [data-block-id]`;

// The row's waits, sized as the timer row's are in `large-documents-build-serves.e2e.ts`: from
// what each wait costs and from what the leg leaves the row. Its page had the editor's first
// batch at 10,904 ms of its own clock on the leg (run 43098661012) and 19,421 ms on this pod,
// with the card drawn, picked and the socket let through before that; its answer left the page
// 78 ms and 54 ms after the press, and the server holds an answer 30 to 180 ms after it (the
// row's own comment). So the card is given 60 s to be on the screen and the first batch 45 s
// from the release, about three times and twice that whole 19.4 s, and the answer 30 s to leave
// and 30 s to reach the server, more than a hundred times what either took.
//
// The row is the leg's second. Ahead of its waits stand the step's 7 s, the timer row's 10.3 s
// and its own seeding, about 5 s; a page that hangs at any one of them fails it 60 s past the
// seeding at the card, or 19.4 + 45 s past it at the first batch - 69.4 s, which ends the step
// at about 92 s of its 240, and at about 205 s with the timer row's own worst case ahead of it.

/** How long the decision row waits for the card it presses to be on the screen. */
const theCardWithinMs = 60_000;

/** How long the decision row waits for the editor's first batch once the socket is let through. */
const theFirstBatchWithinMs = 45_000;

/** How long the decision row waits for the page's answer to leave it, and then for the server
 *  to hold it. */
const theAnswerWithinMs = 30_000;

/** How long after the press the answer's request may take to leave the page. What stands
 *  between the two is the page's own work and none of the build's length: React's state for the
 *  pick, the card's render, and the write the press makes, each a posted task the build has to
 *  let through. Half a second is the span R11 calls a freeze, and it is what a build may not
 *  cost a reader who answers a decision while it runs. */
const theAnswerLeavesWithinMs = 500;

/**
 * Has the page press the Answer button of the card at `card` in the task the editor's first
 * batch lands in, and records what it did: the press the page received, how much of the
 * document the editor held then, whether the build was still running, and when the request
 * carrying the answer left the page.
 *
 * The press is the page's own rather than this side's, for the reason `readerEditsTheBuild`
 * gives: an action driven from here is a round trip, and on a document this size Playwright's
 * own press of the button cost 10.9 s on this pod and 11 to 17 s on GitHub's runners, where the
 * whole build is 1.0 s. A row that presses from here therefore reads whether a round trip beats
 * the build, which says nothing about the reader.
 *
 * It is made from the build's own signal rather than at a moment that happens to fall inside
 * one. The signal is the editor's root taking `data-write-path="live"`, which is the task its
 * first batch's blocks landed in (`live()` in `packages/proof-editor/src/lib.ts`), and this
 * `MutationObserver`'s callback runs at that task's own microtask checkpoint - before the timer
 * the build hands the thread back with, so before every batch after the first
 * (`endOfTask` in `packages/proof-editor/src/progressive-build.ts`). The press therefore lands
 * in the first gap of the build whatever the machine costs: a build of a hundred batches on
 * this pod and one of five on a GitHub runner are the same first gap.
 *
 * The card it presses is the reader's own screen while that build runs: the decision stands in
 * the second section, which the editor's eight-block first batch has not reached, so what holds
 * it is the page's draw of the document's text (`largeDocumentDecidedEarly`). The option has
 * been picked before the build began, so the one thing crossing the build is the press and what
 * the page makes of it, which is what the row times. A card the batch took anyway is said so
 * rather than pressed, since a row that found no button would otherwise read as a build that
 * never came.
 *
 * Install before the navigation; read what it recorded off the page afterwards.
 */
async function theReaderAnswersAtTheFirstBatch(
  page: Page,
  { answer, card }: { answer: string; card: string }
): Promise<void> {
  await page.addInitScript(
    ({
      answered,
      live,
      named,
      pressing,
    }: {
      answered: string;
      live: string;
      named: string;
      pressing: string;
    }) => {
      // When the answer left the page, read where the page makes the request rather than off
      // the browser's resource timing: the number is then the moment the reader's answer went
      // out, not the moment a task the build served recorded that it had.
      window.fetch = new Proxy(window.fetch, {
        apply(made, that, args: [RequestInfo | URL, RequestInit?]) {
          const asked = args[0];
          const where =
            typeof asked === "string" ? asked : asked instanceof URL ? asked.href : asked.url;
          if (new URL(where, window.location.origin).pathname === answered) {
            window.__answerSentMs ??= performance.now();
          }
          return Reflect.apply(made, that, args);
        },
      });
      let answeredAlready = false;
      const press = () => {
        const root = document.querySelector(live);
        if (answeredAlready || root === null) {
          // No batch has landed yet, so there is no build to answer inside.
          return;
        }
        answeredAlready = true;
        const card = document.querySelector(pressing);
        const button = [...(card?.querySelectorAll("button") ?? [])].find(
          (candidate) => (candidate.textContent ?? "").trim() === "Answer"
        );
        if (card === null) {
          window.__answerRefused =
            "the card the reader had picked their option in was off the page by the first batch";
          return;
        }
        if (button === undefined || button.disabled || button.ariaDisabled === "true") {
          window.__answerRefused =
            "the card the reader had picked their option in offered no Answer button to press";
          return;
        }
        button.addEventListener(
          "click",
          (event) => {
            window.__answerPressed = {
              at: event.timeStamp,
              building: document.querySelector(named) === null,
              built: root.querySelector('[contenteditable="true"]')?.childElementCount ?? 0,
            };
          },
          { capture: true, once: true }
        );
        button.click();
      };
      // The document itself, because this script runs before the page has an element to watch.
      new MutationObserver(press).observe(document, {
        attributeFilter: ["data-write-path"],
        attributes: true,
        childList: true,
        subtree: true,
      });
    },
    { answered: answer, live: liveEditorRoot, named: namedEditor, pressing: card }
  );
}

// What a reader's answer costs while their editor is being built. The card is React's - the
// pick is its state, the button's refusal is its state, and the answer is a write the press
// makes - so every step between the press and the request is a posted task the build has to
// let through. The row holds that span to half a second, with the build still running when the
// page received the press: the build may not starve the answer a reader gives it.
//
// Both numbers are the page's own. Until INFRA-341 the row compared the moment the server had
// stored the answer with the moment the editor opened, and that comparison turned on neither:
// the answer is stored 30 to 180 ms after the press in every run, so what decided it was
// whether Playwright's own check and click - 10.9 s on this pod, 11 to 17 s on GitHub's
// runners - beat the build. The press is the page's own now, so no round trip is in it.
//
// What the row arranges is the overlap, which it used to hope for. The press waited for the
// editor's own card, and that card is drawn two posted tasks after the batch that built its
// block - React's render of the host, then the timer one card is drawn in
// (`useAskCardsDrawnInTasks`) - while the build itself is as few as five tasks: its batches
// take 8, 32, 128 and then up to 512 blocks, so a document of 986 of them is five batches on a
// machine fast enough that none of them fills its 120 ms budget. On this pod every batch fills
// it, the build is about a hundred tasks long and the card was drawn well inside it; on a
// GitHub runner the card arrived with the swap, 7,885 ms in, when every one of the 986 blocks
// was built and the editor named, and the guard the row carried then refused the run as it
// should (run 42988104355). Neither machine was wrong about the reader: the row had no window.
//
// So the row takes the window the page gives it. The socket is held while the reader picks
// their option, so nothing of the card's own React is owed when the build starts; the decision
// stands in the second section, which the server drew into the page it sent and the eight-block
// first batch has not reached (`largeDocumentDecidedEarly`); and the press is made from the
// first batch's own signal, in that batch's task
// (`theReaderAnswersAtTheFirstBatch`). The whole of the build is then still to come, on any
// machine, and the line the row prints says how much of it was.
test(`a decision answered while ${documentNames.large}'s editor is built goes out within half a second of the press @phone`, async ({
  browser,
}) => {
  test.setTimeout(900_000);
  const block = decisionBlockId(1);
  const { artifactId, issueKey, path } = await seedDocument(
    "specs-large.md",
    largeDocumentDecidedEarly
  );
  const whole = await topLevelBlocks(artifactId);
  const indexed = (await listIssueAsks(issueKey)).find((ask) => ask.block_id === block);
  expect(
    indexed,
    "the server indexed no ask for the document's first decision block"
  ).toBeDefined();
  const theCard = `${theDraw} [data-testid="ask-${indexed?.id}"]`;

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const build = await watchTheBuildsWindow(page);
    await theReaderAnswersAtTheFirstBatch(page, {
      answer: `/api/v1/asks/${indexed?.id}/answer`,
      card: theCard,
    });
    // Held, so the reader picks their option with no build running and no sync on its way:
    // what crosses the build is the press alone.
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);

    const card = page.locator(theCard);
    await expect(card).toBeVisible({ timeout: theCardWithinMs });
    await card.getByRole("radio", { name: "The next window" }).check();
    await expect(card.getByRole("button", { exact: true, name: "Answer" })).toBeEnabled();
    // And no batch has landed, so nothing below can be read as a press the build had finished
    // before: the editor's root is in the page from the construction, with none of the document
    // in it (`large-documents-build.e2e.ts`).
    expect(await page.locator(liveEditorBlocks).count()).toBe(0);

    // Throttled here and not before the load: the build the press crosses begins with the
    // release below, and the boot, the draw and the reader's own picking above it are setup no
    // reading of this row is taken of (`throttleTheCpu`).
    await throttleTheCpu(page);
    await transport.release();
    const firstBatchMs = await build.firstBatch(theFirstBatchWithinMs);
    // The same selector once the first batch is in: it finds that batch's blocks, so the none
    // it counted before the release was an editor holding none.
    expect(
      await page.locator(liveEditorBlocks).count(),
      "the first batch is in, and the selector above counts none of its blocks: its count of " +
        "none before the release proved nothing"
    ).toBeGreaterThan(0);

    // The page has pressed the card and its answer has left - or it has said why it pressed
    // nothing, which is read first so a refusal is the failure rather than the wait that
    // follows it. The wait is for the request, not for the build: what the editor does with
    // the rest of the document after this is the business of the timer row of
    // `large-documents-build-serves.e2e.ts` and of `large-documents-long-build.e2e.ts`.
    await expect
      .poll(
        async () =>
          await page.evaluate(
            () => window.__answerSentMs !== undefined || window.__answerRefused !== undefined
          ),
        {
          intervals: [250],
          message: "the page never answered the decision block it had been given",
          timeout: theAnswerWithinMs,
        }
      )
      .toBe(true);
    expect(
      await page.evaluate(() => window.__answerRefused),
      "the page refused to press the card it was given"
    ).toBeUndefined();
    // And the server holds it, which is what the press was for.
    await expect
      .poll(
        async () => (await listIssueAsks(issueKey)).find((ask) => ask.block_id === block)?.state,
        {
          intervals: [250],
          message: "the answer never reached the server",
          timeout: theAnswerWithinMs,
        }
      )
      .toBe("answered");

    const pressed = await page.evaluate(() => window.__answerPressed);
    const sentMs = (await page.evaluate(() => window.__answerSentMs)) ?? Number.NaN;
    const leftAfterMs = sentMs - (pressed?.at ?? Number.NaN);
    // Where the build had got to by the time the server held the answer, which is the margin
    // the press was made with: read now rather than waited for, since what the build does after
    // the answer is the business of the timer row of `large-documents-build-serves.e2e.ts` and
    // of `large-documents-long-build.e2e.ts`.
    const reachedMs = await page.evaluate(() => performance.now());
    const window_ = await build.window();
    // Printed before either number is read against anything, as every other number in this
    // file is.
    console.log(
      `R11 decision during the build · the editor's first batch landed at ` +
        `${Math.round(firstBatchMs)} ms and the page received the press at ` +
        `${Math.round(pressed?.at ?? Number.NaN)} ms, ` +
        `${Math.round((pressed?.at ?? Number.NaN) - firstBatchMs)} ms into the build, with ` +
        `${pressed?.built ?? 0} of ${whole} block(s) built and the build ` +
        `${pressed?.building === true ? "running" : "over"}; the answer's request left at ` +
        `${Math.round(sentMs)} ms, ${Math.round(leftAfterMs)} ms later (target ` +
        `${theAnswerLeavesWithinMs} ms); and at ${Math.round(reachedMs)} ms, with the server ` +
        `holding the answer, the editor was ${
          window_.namedMs === undefined
            ? "still being built"
            : `named at ${Math.round(window_.namedMs)} ms`
        }. Every number is the page's own clock.`
    );

    expect(pressed, "the page never pressed the card's Answer button").toBeDefined();
    // And the press was made during a build rather than after one. That the editor was not yet
    // named is construction - the page presses from the first batch's own signal, in that
    // batch's task (`theReaderAnswersAtTheFirstBatch`) - but "not yet named" is not "more
    // blocks to come": a first batch holding the whole document meets it, and over such a run
    // the half second below would be a reading of an idle page. What the build still owed is
    // the document's own blocks against the ones the editor held at the press.
    expect(
      pressed?.built ?? whole,
      "the first batch was the whole document, so this press was not made during a build"
    ).toBeLessThan(whole);
    expect(
      Math.round(leftAfterMs),
      `the reader's answer took ${Math.round(leftAfterMs)} ms to leave the page after the ` +
        "press: the build is holding the one thing the reader did with it"
    ).toBeLessThanOrEqual(theAnswerLeavesWithinMs);
  } finally {
    await alice.close();
  }
});
