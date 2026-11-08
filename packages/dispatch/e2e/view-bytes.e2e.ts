/**
 * What a `view` block costs the reader in JavaScript (dispatch://INFRA-682, A5): the bytes a
 * document holding one makes the browser fetch, less what the same document as a plain
 * paragraph makes it fetch at the same build.
 *
 * One row, in a file of its own, because the file is how a project selects it: this one is
 * `chromium-alone`'s (`testMatch`, e2e/playwright.config.ts), which Playwright starts only once
 * every four-lane project has finished and which runs one row at a time with `retries: 0`. What
 * a page gets fetched before the row looks is read against the machine, and three other lanes
 * on it change that number; a retry would turn a budget a change half passes into a green run.
 * The frame rows of `view-budget.e2e.ts` beside it stay in the four-lane `chromium` project,
 * where a red elsewhere cannot leave them unrun: they read what the frames of one page asked
 * for, which four lanes do not change.
 *
 * It is Chromium's alone, and in no other engine's `testMatch` or `grep`: the row reads two
 * pages of one build against each other, so a second engine would only repeat that subtraction
 * in its own chunking.
 */
import { type Browser, expect, type Page, type Request, test } from "@playwright/test";

import { documentEditor } from "./editor";
import { resetDatabase } from "./seed";
import { asUser } from "./users";
import {
  documentPath,
  seedReadyView,
  VIEW_CAPTION,
  viewBlock,
  viewDirective,
} from "./view-fixtures";

/**
 * What R6 allows a `view` block to cost the reader, over what the same document without one
 * costs at the same build. It read 10,000, then 15,000, and is now the 16,000 a person approved
 * (dispatch://INFRA-682/ask/ac338237-efb6-4ec0-b477-6ea4da57cd49, the spec approved at version
 * 13). Each rise was measured before it was asked for rather than after: 10,000 would have cost
 * the four-frame budget and the repaint splice, which together bought 1,434 bytes and still left
 * the change 2,646 over while making a streamed agent turn rebuild a block's frame on every
 * repaint; and 15,000 would have cost about 500 bytes more from a list of parts every one of
 * which is a message a reader meets or a limit already kept - the two fixes a review found, a
 * page mid-navigation still receiving data and an older version arriving last, are among the
 * bytes this last rise paid for.
 *
 * This number bounds the SAME-BUILD difference, which is the form that can live on `main`. A5's
 * own sentence bounds the cross-build growth against the commit before the change, which counts
 * every file such a document loads, so moving code between chunks changes it by nothing; that
 * measurement is made by hand per round and quoted on the pull request.
 */
const BUDGET_BYTES = 16_000;

/** The row's document with the block written as the plain paragraph it would be without one.
 *  What this one costs the reader is what the block's own cost is measured against. */
const BASE_DOCUMENT = `# Loop\n\n${VIEW_CAPTION}\n`;

/**
 * What a page fetched of its own JavaScript, as the row reads it. An address is counted once
 * however often the page asks for it - the second ask is answered from the browser's own cache,
 * and the reader pays for the bytes once - so the sum is a property of the page's module graph
 * rather than of how many times something was fetched.
 */
interface Traffic {
  /** The `.js` addresses whose responses the page has read. */
  answered(): ReadonlySet<string>;
  /** How many `.js` requests the page has sent and not yet seen answered or refused. */
  asking(): number;
  /** The sum of the bodies of the answers, one body per address. */
  bytes(): Promise<number>;
}

function watchJavaScript(page: Page): Traffic {
  const sizes = new Map<string, Promise<number>>();
  const asked = new Set<Request>();
  page.on("request", (request) => {
    if (new URL(request.url()).pathname.endsWith(".js")) {
      asked.add(request);
    }
  });
  page.on("requestfailed", (request) => asked.delete(request));
  page.on("requestfinished", (request) => {
    asked.delete(request);
    const url = request.url();
    if (!new URL(url).pathname.endsWith(".js") || sizes.has(url)) {
      return;
    }
    sizes.set(
      url,
      request.sizes().then((measured) => measured.responseBodySize)
    );
  });
  return {
    answered: () => new Set(sizes.keys()),
    asking: () => asked.size,
    bytes: async () => (await Promise.all(sizes.values())).reduce((total, size) => total + size, 0),
  };
}

/**
 * The scripts the document itself says it pulled in: the entry `<script src>` of index.html, and
 * the `<link rel="modulepreload">` Vite's preload helper writes for a chunk's own imports before
 * it imports that chunk. It is not the whole set - a chunk whose imports are all loaded already
 * is fetched by the `import()` with no link of its own, as `features/doc/pending-edits` is here -
 * but it is the part of the set the document states before the fetches answer, which is what
 * gives the wait below something to wait for.
 */
async function declaredScripts(page: Page): Promise<string[]> {
  return await page.evaluate(() =>
    Array.from(
      document.querySelectorAll<HTMLLinkElement | HTMLScriptElement>(
        'script[src], link[rel="modulepreload"][href]'
      ),
      (element) => (element instanceof HTMLScriptElement ? element.src : element.href)
    ).filter((url) => new URL(url).pathname.endsWith(".js"))
  );
}

/**
 * The bytes of JavaScript the page fetched, and how many addresses they came from.
 *
 * What ends the wait is a set of responses rather than a span of quiet: every script the
 * document declares has answered, and no script the page asked for is still being answered. The
 * declared set is read again at every poll, so a chunk declared late is waited for rather than
 * missed, and the whole condition is read once more after the sum is taken - a measure under
 * load either counts every byte or leaves the row red, and never quietly drops a chunk that was
 * still arriving. A quiet window did the last of those: it ended 500 ms after the last request
 * settled, counted whatever had finished by then, and moved by a whole chunk from machine to
 * machine.
 */
async function javaScriptBytes(
  page: Page,
  traffic: Traffic
): Promise<{ bytes: number; scripts: number }> {
  const declared = new Set<string>();
  const outstanding = async (): Promise<string[]> => {
    for (const url of await declaredScripts(page)) {
      declared.add(url);
    }
    const answered = traffic.answered();
    const waiting = [...declared].filter((url) => !answered.has(url));
    const inFlight = traffic.asking();
    return (inFlight === 0 ? waiting : [...waiting, `${inFlight} in flight`]).sort();
  };
  await expect.poll(outstanding, { intervals: [100], timeout: 30_000 }).toEqual([]);
  const counted = [...traffic.answered()].sort();
  const bytes = await traffic.bytes();
  expect(
    await outstanding(),
    "A5: the page asked for no further script while the sum was taken"
  ).toEqual([]);
  expect(
    [...traffic.answered()].sort(),
    "A5: the sum is of every script the page fetched, and the page fetched no other since"
  ).toEqual(counted);
  return { bytes, scripts: counted.length };
}

/**
 * What one reader fetched of the page at `path`, in a browser context of its own.
 *
 * The context is what holds two measurements apart. Both are taken at one build from one server,
 * so a second reader sharing the first's HTTP cache would be answered out of it and fetch almost
 * nothing: the subtraction below would read as the block's chunks alone, or as nothing at all,
 * and a row that measures nothing passes. `asUser` opens a context of its own per reader, with a
 * cache of its own, and the row proves it rather than assuming it - it reads one document in two
 * readers and holds them to the same bytes, which a shared cache makes impossible.
 */
async function readerFetched(
  browser: Browser,
  path: string,
  drawn: (page: Page) => Promise<void>
): Promise<{ bytes: number; scripts: number }> {
  const reader = await asUser(browser, "alice");
  try {
    const page = await reader.newPage();
    const traffic = watchJavaScript(page);
    await page.goto(path);
    await drawn(page);
    return await javaScriptBytes(page, traffic);
  } finally {
    await reader.close();
  }
}

test.beforeEach(async () => {
  await resetDatabase();
});

/**
 * A5 at one build: what a `view` block costs the reader is what the document holding one makes
 * the browser fetch, less what the same document as a plain paragraph makes it fetch. Both pages
 * are of this build and this server, so the row pins no byte count and names no commit: the
 * number the budget is read against is measured beside the number it bounds, and `main`'s own
 * growth moves both of them together and the difference not at all. What A5 exists to catch -
 * the block's own code growing - is the difference, and that is what is bounded. What the
 * difference cannot hold is code of the block that lands in a chunk every document fetches,
 * which a reader of the paragraph pays for too; a pin on one page's whole sum held that, and
 * held every unrelated byte `main` grew along with it, which is why it could not stay.
 *
 * The paragraph is read twice, by two readers, and both readings must agree to the byte. That is
 * the control: a reader answered out of another's cache reads far less than one that fetched the
 * page, so two equal readings say the two caches are apart. The second reading is taken after
 * the block's, so it says as well that the block's reader left nothing behind for it.
 *
 * The document without the block declares fewer scripts, since nothing on it imports the block's
 * chunks; the line the row prints holds both counts and both sums, so a reader of the output can
 * see that each number is a whole page rather than a remainder of the one before it.
 */
test("a view block costs the reader at most 16,000 more bytes of JavaScript", async ({
  browser,
}) => {
  const fixture = await seedReadyView();
  const withBlock = await documentPath(
    viewDirective({ caption: VIEW_CAPTION, data: fixture.dataRef, page: fixture.pageRef }),
    "Budget"
  );
  const asParagraph = await documentPath(BASE_DOCUMENT, "Budget as a paragraph");
  const blockDrawn = async (page: Page): Promise<void> => {
    await expect(viewBlock(page, VIEW_CAPTION).frameLocator("iframe").locator("#words")).toHaveText(
      "drawn"
    );
  };
  const paragraphDrawn = async (page: Page): Promise<void> => {
    await expect(documentEditor(page)).toContainText(VIEW_CAPTION);
  };

  const paragraph = await readerFetched(browser, asParagraph, paragraphDrawn);
  const block = await readerFetched(browser, withBlock, blockDrawn);
  const paragraphAgain = await readerFetched(browser, asParagraph, paragraphDrawn);
  const cost = block.bytes - paragraph.bytes;

  // The line the pull request quotes: both sums, both script counts, and what they differ by.
  console.log(
    `view budget: with the block ${block.bytes} bytes of JavaScript in ${block.scripts} ` +
      `scripts; as a paragraph ${paragraph.bytes} bytes in ${paragraph.scripts} scripts, and ` +
      `again ${paragraphAgain.bytes} bytes in ${paragraphAgain.scripts} scripts; the block ` +
      `costs ${cost}`
  );

  expect(
    paragraphAgain,
    "A5: two readers of the paragraph fetched the same bytes, so neither was answered out of " +
      "the other's cache"
  ).toEqual(paragraph);
  expect(
    paragraph.bytes,
    "A5: the paragraph's reader fetched a whole page of JavaScript, not a remainder of the block's"
  ).toBeGreaterThan(BUDGET_BYTES * 10);
  expect(
    block.scripts,
    "A5: the document holding the block fetched scripts the paragraph's reader did not"
  ).toBeGreaterThan(paragraph.scripts);
  expect(
    cost,
    "A5: the two readings differ by the block's own chunks, so the difference is what the block " +
      "costs rather than a remainder"
  ).toBeGreaterThan(0);
  expect(
    cost,
    `A5: a view block costs the reader ${cost} bytes of JavaScript: ${block.bytes} with it ` +
      `against ${paragraph.bytes} without it, at one build`
  ).toBeLessThanOrEqual(BUDGET_BYTES);
});
