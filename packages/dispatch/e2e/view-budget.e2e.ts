/**
 * The four rules that keep a page of `view` blocks from running them all
 * (dispatch://INFRA-682, A5): a frame starts only after the text around it shows and only near
 * the screen, at most four run at once, and a frame more than three screen heights away is
 * removed.
 *
 * This file runs in Chromium alone, and in no other engine's `testMatch` or `grep`: its rows
 * rest on `IntersectionObserver` margins, on scroll positions the test sets and on what the
 * frames of one page asked for, which the `view-block`, `view-sandbox` and `view-grants` rows
 * already exercise per engine.
 *
 * Every row here stays in the four-lane `chromium` project, where a red elsewhere cannot leave
 * it unrun: what each reads is one page's own frames, which three other lanes on the machine do
 * not change. The byte measure that was written beside them is read against the machine and has
 * a file of its own for it, `view-bytes.e2e.ts`, which `chromium-alone` runs with the machine to
 * itself (e2e/playwright.config.ts, above `aloneProjects`).
 */
import { expect, type Page, type Request, test } from "@playwright/test";

import {
  createArtifactGrant,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  createProjectDocument,
  uploadIssueFile,
} from "./api";
import { resetDatabase } from "./seed";
import { asUser } from "./users";
import {
  openCommentThreadReplies,
  seedViewIssue,
  seedViewProject,
  type ViewFixture,
  viewBlock,
  viewDirective,
} from "./view-fixtures";

const CAPTION = "Attempts of the kernel boot loop.";

/** A paragraph per line, enough of them to put a block several screen heights down. */
function filler(lines: number): string {
  return Array.from(
    { length: lines },
    (_, index) => `Line ${index + 1}: the ledger and the mirror each keep a clock of their own.`
  ).join("\n\n");
}

/** A page file that answers `ready` and says nothing else, and a data file granted to it. */
async function seedView(): Promise<ViewFixture> {
  await seedViewProject();
  return seedViewIssue({
    html:
      '<!doctype html>\n<html lang="en"><body><p id="words">drawn</p>' +
      '<script>parent.postMessage({ type: "dispatch-view:ready" }, "*");</script></body></html>\n',
    title: "Kernel boot loop",
  });
}

/** `content` as a project document named `name`, opened in `page`. */
async function openDocument(page: Page, content: string, name: string): Promise<void> {
  const document = await createProjectDocument("CORE", { content, name });
  await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);
}

test.beforeEach(async () => {
  await resetDatabase();
});

test("a view block far down the page reads nothing until the reader scrolls to it", async ({
  browser,
}) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const reads: string[] = [];
    page.on("request", (request) => {
      const path = new URL(request.url()).pathname;
      if (path.includes("/api/v1/artifacts/") && path.includes("/versions/")) {
        reads.push(path);
      }
    });
    await openDocument(
      page,
      `${filler(220)}\n\n${viewDirective({
        caption: CAPTION,
        data: fixture.dataRef,
        page: fixture.pageRef,
      })}`,
      "Far below"
    );
    const block = viewBlock(page, CAPTION);
    await expect(page.getByText("Line 1:", { exact: false }).first()).toBeVisible();
    const away = async () =>
      block.evaluate((element) => element.getBoundingClientRect().top / window.innerHeight);
    expect(
      await away(),
      "A5: the block starts more than one screen height below the screen"
    ).toBeGreaterThan(1);

    await page.waitForTimeout(2_000);
    expect(reads, "A5: nothing of the page file or the data file was read").toEqual([]);
    await expect(block.locator("iframe"), "A5: and no frame was inserted").toHaveCount(0);

    await block.scrollIntoViewIfNeeded();
    await expect(
      block.frameLocator("iframe").locator("#words"),
      "A5: scrolling to the block starts its frame"
    ).toHaveText("drawn");
    expect(reads.length, "A5: and only then is anything read").toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

test("a view block more than three screen heights away loses its frame, and takes it back", async ({
  browser,
}) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      `${filler(400)}\n\n${viewDirective({
        caption: CAPTION,
        data: fixture.dataRef,
        page: fixture.pageRef,
      })}`,
      "Scrolled away"
    );
    const block = viewBlock(page, CAPTION);
    await block.scrollIntoViewIfNeeded();
    await expect(block.frameLocator("iframe").locator("#words")).toHaveText("drawn");

    await page.getByText("Line 1:", { exact: false }).first().scrollIntoViewIfNeeded();
    const away = await block.evaluate(
      (element) => element.getBoundingClientRect().top / window.innerHeight
    );
    expect(away, "A5: the reader is now more than three screen heights away").toBeGreaterThan(3);
    await expect(block.locator("iframe"), "A5: so the frame is removed").toHaveCount(0, {
      timeout: 10_000,
    });

    await block.scrollIntoViewIfNeeded();
    await expect(
      block.frameLocator("iframe").locator("#words"),
      "A5: and it starts again when the block comes back"
    ).toHaveText("drawn");
  } finally {
    await alice.close();
  }
});

test("a view block asks for its page only after its caption is on the page", async ({
  browser,
}) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The block's first read of its page is the `HEAD` of the page route, which goes through
    // `fetch`. What the document held at that moment is what this records.
    await page.addInitScript(() => {
      const seen: (string | null)[] = [];
      Object.assign(window, { __whenThePageWasAskedFor: seen });
      const original = window.fetch;
      // `Object.assign` keeps whatever else the engine hangs off `fetch`, which is what its own
      // type requires of a replacement.
      window.fetch = Object.assign(
        (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
          const address =
            input instanceof URL
              ? input.href
              : input instanceof Request
                ? input.url
                : String(input);
          if (address.endsWith("/page")) {
            seen.push(
              document.querySelector('[data-proof-block-type="view"] [data-proof-block-content]')
                ?.textContent ?? null
            );
          }
          return original.call(window, input, init);
        },
        original
      );
    });
    await openDocument(
      page,
      viewDirective({ caption: CAPTION, data: fixture.dataRef, page: fixture.pageRef }),
      "Caption first"
    );
    await expect(viewBlock(page, CAPTION).frameLocator("iframe").locator("#words")).toHaveText(
      "drawn"
    );
    const captions = await page.evaluate<(string | null)[]>(() =>
      Reflect.get(window, "__whenThePageWasAskedFor")
    );
    expect(captions.length, "A5: the page was asked for").toBeGreaterThan(0);
    expect(
      captions[0],
      "A5: the block's caption was already on the page when its frame was asked for"
    ).toContain(CAPTION);
  } finally {
    await alice.close();
  }
});

test("a view block waits its turn where six of them sit on one screen", async ({ browser }) => {
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await openDocument(
      page,
      Array.from({ length: 6 }, (_, index) =>
        viewDirective({
          caption: `Attempts, block ${index + 1}.`,
          data: fixture.dataRef,
          page: fixture.pageRef,
        })
      ).join("\n"),
      "Six blocks"
    );
    await expect(
      page.locator('section[data-proof-block-type="view"]'),
      "A5: six blocks"
    ).toHaveCount(6);
    await expect(
      page.locator('section[data-proof-block-type="view"] iframe'),
      "A5: four frames run at once"
    ).toHaveCount(4, { timeout: 30_000 });
    await expect(
      page.getByText("waiting", { exact: true }),
      "A5: and the other two wait their turn"
    ).toHaveCount(2);
  } finally {
    await alice.close();
  }
});

/** A block's frame asking for its page: `…/versions/<n>/page`, the one route a frame navigates
 *  to. Both counts below read the method as well, and count the `GET`s alone: the block's own
 *  `HEAD` of that address is answered with a length and no body, which Chromium reports as an
 *  aborted request although the `fetch` resolved and the block read its answer. What this row
 *  is about is the frame's own navigation, which is the `GET`. */
const PAGE_ROUTE = /^\/api\/v1\/artifacts\/[^/]+\/versions\/\d+\/page$/;

/** The bodies T8's page draws a block in. The Conversation draws them in two places at once:
 *  the feed holds the two messages' turns and the ask's, and the margin beside it holds the
 *  comment thread - the comment and its reply - and the ask's own card. */
const BODIES = ["a comment", "a reply", "a message", "an ask", "another message"] as const;

/** The blocks those bodies put on the page: one each, and the ask's question a second time,
 *  since the ask is drawn both in its turn in the feed and in its card in the margin. Each copy
 *  is a block of its own, with a frame slot of its own under the budget. */
const BLOCKS = BODIES.length + 1;

/** What a `view` page file holds for the row below: it answers `ready`, so the block stops
 *  saying it got no ready message, and reports a height of its own, which is what resizes the
 *  slot, moves every block under it and makes the budget rank its frames again. */
function heightReportingPage(words: string, height: number): string {
  return (
    '<!doctype html>\n<html lang="en"><body><p id="words">' +
    `${words}</p><script>parent.postMessage({ type: "dispatch-view:ready" }, "*");` +
    `parent.postMessage({ height: ${height}, type: "dispatch-view:height" }, "*");` +
    "</script></body></html>\n"
  );
}

/**
 * An issue whose Conversation draws one `view` block in each of five bodies at once - a comment,
 * its reply, two messages and an ask - against the four frames of the budget. Each block names a
 * page file of its own, so a count of page-route requests is a count per block rather than one
 * address asked for many times, and each of those pages reports a height of its own.
 */
async function seedBodies(): Promise<{ comment: string; issue: string }> {
  await seedViewProject();
  const issue = await createIssue({ project: "CORE", title: "Kernel boot loop" });
  await uploadIssueFile(issue.key, {
    content: '[{"label":"attempts","value":7}]',
    name: "loop.json",
    type: "application/json",
  });
  const dataRef = `dispatch://${issue.key}/artifact/loop-json`;
  const directives: string[] = [];
  for (const [index, where] of BODIES.entries()) {
    const file = await uploadIssueFile(issue.key, {
      content: heightReportingPage(`drawn in ${where}`, 140 + index * 60),
      name: `chart-${index + 1}.html`,
      type: "text/html; charset=utf-8",
    });
    await createArtifactGrant(file.artifact.id, dataRef);
    directives.push(
      viewDirective({
        caption: `Attempts, in ${where}.`,
        data: dataRef,
        page: `dispatch://${issue.key}/artifact/${file.artifact.slug}`,
      })
    );
  }
  const session = { actor: { id: "e2e-view", kind: "session" as const }, as: "agent" as const };
  const comment = await createComment(issue.key, { body: directives[0] }, session);
  await createComment(issue.key, { body: directives[1], reply_to: comment.id }, session);
  await createMessage(issue.key, { body: directives[2] }, session);
  await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: directives[3] },
    session
  );
  await createMessage(issue.key, { body: directives[4] }, session);
  return { comment: comment.id, issue: issue.key };
}

// T8, the check that tells the rebuild race from a working budget. Every other frame row here
// draws its blocks from one document body, which mounts them in one pass and in document order.
// This one opens the page that bit: a Conversation whose feed holds a message, an ask and
// another message, and whose margin beside it holds a comment, that comment's reply and the
// ask again - several containers at once, each mounting its blocks as its own reads answer,
// and each frame that reports a height moving the blocks under it and having the budget rank
// them again. Ranking by the order the slots happened to register, and
// taking a slot from a frame still fetching its page, together abort that navigation: the
// engine reports the abort as a `load`, the block starts its ready timer over a frame that is
// gone, and the reader is left with an empty frame and a line saying the page sent no ready
// message (dispatch://INFRA-682). So this row counts what the frames asked for: one page route
// per block at most, and no request that failed. It runs in `chromium` alone, as this file's
// header says of every row in it - what it reads is each engine's `load` of an aborted
// navigation, which every engine reports, and the page it opens needs no engine of its own.
test("a view block in each of a page's bodies asks for its page once, and loses no frame", async ({
  browser,
}) => {
  const fixture = await seedBodies();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const asked: string[] = [];
    const failed: string[] = [];
    /** When the page route was last asked for, or last failed: the Conversation holds a live
     *  connection open for as long as the reader is on it, so the quiet this row waits for is
     *  the quiet of the one route it counts, not of the page's traffic. */
    let lastAt = Date.now();
    const pageRoute = (request: Request): string | undefined => {
      const path = new URL(request.url()).pathname;
      return PAGE_ROUTE.test(path) ? path : undefined;
    };
    page.on("request", (request) => {
      const path = pageRoute(request);
      if (path !== undefined && request.method() === "GET") {
        asked.push(path);
        lastAt = Date.now();
      }
    });
    page.on("requestfailed", (request) => {
      const path = pageRoute(request);
      if (path !== undefined && request.method() === "GET") {
        failed.push(`${path} ${request.failure()?.errorText ?? "failed"}`);
        lastAt = Date.now();
      }
    });

    await page.goto(`/issues/${fixture.issue}/conversation`);
    await openCommentThreadReplies(page, fixture.comment);
    const blocks = page.locator('section[data-proof-block-type="view"]');
    await expect(
      blocks,
      "A5: the five bodies, and the quote its reply carries, hold six blocks"
    ).toHaveCount(BLOCKS);
    await expect(
      page.locator('section[data-proof-block-type="view"] iframe'),
      "A5: four of the six run, the budget's four"
    ).toHaveCount(4, { timeout: 30_000 });

    // Every frame that runs drew its own page, so what the counts below read is a page of
    // blocks that got as far as their pages rather than blocks that got nowhere.
    let drawn = 0;
    for (let index = 0; index < BLOCKS; index += 1) {
      const block = blocks.nth(index);
      if ((await block.locator("iframe").count()) === 0) {
        continue;
      }
      await expect(
        block.frameLocator("iframe").locator("#words"),
        `A5: the frame of block ${index + 1} drew its page`
      ).toHaveText(/^drawn in /, { timeout: 30_000 });
      drawn += 1;
    }
    expect(drawn, "A5: the four frames of the budget each drew their page").toBe(4);

    // No frame has asked for a page in two seconds, so the ranking has settled and nothing more
    // is on its way: two seconds is far longer than this server takes to answer that route, so
    // a frame started again after an eviction is counted rather than missed. A page that never
    // goes quiet is the same race, and fails here.
    await expect
      .poll(() => Date.now() - lastAt, { intervals: [250], timeout: 15_000 })
      .toBeGreaterThanOrEqual(2_000);
    // The two runs the pull request quotes for this row are this line, with the fix and without.
    console.log(
      `view bodies: ${asked.length} page requests and ${failed.length} failed, for ${BLOCKS} blocks`
    );
    expect(failed, "A5: no frame's page request failed, so none was torn out mid-load").toEqual([]);
    expect(
      asked.length,
      `A5: ${asked.length} page requests for ${BLOCKS} blocks: ${asked.join(", ")}`
    ).toBeLessThanOrEqual(BLOCKS);
    await expect(
      page.getByText("sent no ready message", { exact: false }),
      "A5: and no block was left saying its page sent no ready message"
    ).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

/**
 * The bound on how long a frame that is still loading keeps the budget slot it holds
 * (`frame-budget.ts`'s `PAGE_DEADLINE_MS`). A loading frame keeps its slot so that no ranking
 * aborts a navigation in flight, and a navigation that never completes - a stalled connection on
 * the page route, which is no error page and so fires no `load` - would keep it for the life of
 * the page: four such frames would stop every other block on the page for ever. The block's own
 * stall protection is no cover for that window, since its ready deadline starts at the `load`
 * that never comes.
 *
 * Every `GET` of the page route here hangs: it is intercepted and never answered, which is that
 * stalled connection. The `HEAD` the block sends first is answered as ever, so each block gets
 * as far as inserting a frame and holding a slot with it.
 */
test("a view block waiting behind frames whose pages never answer still gets its turn", async ({
  browser,
}) => {
  // This row waits out the budget's own load deadline, ten seconds, after a document of six
  // blocks has drawn: that deadline is what the row observes, and the default thirty seconds
  // leaves it no room (e2e/playwright.config.ts, above `expectTimeout`).
  test.setTimeout(90_000);
  const fixture = await seedView();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.route(
      (url) => PAGE_ROUTE.test(url.pathname),
      async (route, request) => {
        // A `GET` is answered by nobody, so the frame making it loads until the page closes.
        if (request.method() !== "GET") {
          await route.fallback();
        }
      }
    );
    await openDocument(
      page,
      Array.from({ length: 6 }, (_, index) =>
        viewDirective({
          caption: `Attempts, block ${index + 1}.`,
          data: fixture.dataRef,
          page: fixture.pageRef,
        })
      ).join("\n"),
      "Pages that hang"
    );
    const frames = page.locator('section[data-proof-block-type="view"] iframe');
    await expect(
      frames,
      "A5: the four blocks nearest the screen insert their frames, and those hang"
    ).toHaveCount(4, { timeout: 30_000 });
    const below = [5, 6].map((number) => viewBlock(page, `Attempts, block ${number}.`));
    for (const [index, block] of below.entries()) {
      await expect(
        block.getByText("waiting", { exact: true }),
        `A5: block ${index + 5} waits its turn behind them`
      ).toBeVisible();
    }

    // The reader scrolls to the two that waited: they are on the screen and the hanging frames
    // are not, so the one thing between each of them and a frame of its own is the slot a frame
    // that will never load is holding.
    await below[1].scrollIntoViewIfNeeded();
    for (const [index, block] of below.entries()) {
      await expect(
        block.locator("iframe"),
        `A5: block ${index + 5} takes a slot once a hanging frame's deadline is up`
      ).toHaveCount(1, { timeout: 30_000 });
    }
    await expect(frames, "A5: and four frames run, no more").toHaveCount(4);
  } finally {
    await alice.close();
  }
});
