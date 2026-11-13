// What a reader of a large document is given before their editor, and what the page holds back
// until they have one.
//
// The document's own reads are started from the address, the first of them before any of the
// page's own code has evaluated, so they wait for nothing the entry chunk or the issue page
// does; the first block is in the page's own bytes, drawn there by the Go server
// (`internal/dispatch/pmdoc/first_screen.go`), so it owes nothing at all to the page's own
// code; the margin beside the document draws no card a reader cannot see; and the reads and the
// chrome that fill the rest of the issue page go out and are drawn after the editor's first
// batch (`web/src/features/doc/first-batch.ts`, `web/src/features/doc/AfterTheFirstBatch.tsx`).
// None of these rows is timed: each reads what the page asked for, what it drew and which side
// of the first batch either fell on, never how long any of it took. The numbers that load is
// mostly made of are in `large-documents-measured.e2e.ts`.
//
// Their documents are short ones, because an order is the same over any length of document. The
// one exception is the row that reads the reader's place half-way down the larger document of
// `large-documents.ts`: a panel landing under a reader who has placed themselves needs a page
// tall enough for them to have read their way into it.
//
// The numbers R11's targets are read from are in `large-documents-measured.e2e.ts`, and the
// rows beside them that need a build of the larger document to last are in
// `large-documents-build-serves.e2e.ts`, `large-documents-desktop-build.e2e.ts` and
// `large-documents-long-build.e2e.ts`; what stands on the reader's screen while the editor is
// built is in `large-documents-build.e2e.ts`; what a reader does with a document whose editor
// is still being built is in `large-documents-reader.e2e.ts`; where the chrome these rows draw
// leaves the server's first block, a place rather than an order, is in
// `large-documents-fold.e2e.ts`. What the eight files share is
// `large-documents-rows.ts`.
import { expect, type Page, test } from "@playwright/test";

import { LONGEST_HOLD_MS } from "../web/src/features/doc/first-batch";
import { subscribeSession } from "./agents";
import { createAsk, patchIssue } from "./api";
import { documentEditor, documentTransport, editorBeingBuilt } from "./editor";
import {
  documentDiagrams,
  documentTitle,
  largeDocument,
  proseOnlyDocument,
  sectionHeading,
  smallDocument,
} from "./large-documents";
import {
  type PageRead,
  readTheChrome,
  readTheReads,
  watchTheChrome,
  watchTheReads,
} from "./large-documents-after-the-batch";
import { agent, seedDocument, shortDocument } from "./large-documents-rows";
import { settledScrollTop, withoutScrollAnchoring } from "./mermaid-helpers";
import { expectNoneOf } from "./none-of";
import { asUser } from "./users";

// Every wait of this file for something the page draws is bounded by one of the two figures
// below, so a page that never draws it fails the row by name inside the leg it runs in.
//
// The leg. These rows run in the shards, `chromium` and `iphone` four workers wide, under a step
// of 540 s with one retry (.github/workflows/dispatch-browser.yaml). In run 43421318396 the
// `chromium` rows were in `shard (3)`, whose step took 329 s, and the `iphone` rows in
// `shard (4)` and `shard (5)`, 369 s and a projected 372 s since that leg's moves; the longest
// leg of that run took 433 s, which leaves 107 s. A row that hangs runs twice, so it adds twice
// its failing time, less what it costs when it passes, to the worker it runs on.
//
// What the waits cost, timed on this pod, one worker on two CPUs (dispatch://INFRA-341), the
// slower of `chromium` and `iphone`: every wait bounded by `aDrawWithinMs` - the draw's first
// block, the editor's first block, the held boxes, the Subscribers control, the headers, the
// reads and parts the page held behind its first batch - took 0.03 to 2.67 s. The two bounded
// by `aLongDrawWithinMs` took 12.98 s (the editor open over the larger document, from the
// navigation) and 12.63 s (the chrome on a page whose socket never answers, which waits out
// `LONGEST_WAIT_FOR_THE_DOCUMENT_MS`).
//
// So 20 s is more than seven times the slowest of the first kind and 40 s three times the
// slowest of the second. The longest a row then runs before it fails is the long-document row's:
// the 9.6 s it spends before its editor wait and 40 s, 49.6 s; twice that, less the 22.6 s the
// row costs passing, is 76.6 s more on one worker, which fits every leg above, the longest's
// 107 s among them.

/** How long a row waits for something the page draws in its first seconds. */
const aDrawWithinMs = 20_000;

/** How long a row waits for the editor over the larger document, or for the chrome a page whose
 *  socket never answers draws once its own deadline has passed. */
const aLongDrawWithinMs = 40_000;

// The three rows below are about what the page does in front of the document's own draw,
// which is what the numbers of `large-documents-measured.e2e.ts` are mostly made of. The
// document's own reads are started from the address, the first of them before any of the
// page's own code has evaluated, so they wait for nothing the entry chunk or the issue page
// does; and the margin beside the document draws no card a reader cannot see. None of the
// three is timed: each reads what the page asked for and what it drew, never how long either
// took.

/** The pathname of each request the page made, newest last, for a row that reads what the page
 *  asked for. */
function requestedPaths(page: Page): string[] {
  const paths: string[] = [];
  page.on("request", (request) => paths.push(new URL(request.url()).pathname));
  return paths;
}

/** Either route a document's markdown is read by: the one the address names it at, and the one
 *  the issue's own read names it at (`web/src/api/client.ts`). */
const documentTextRoute = /^\/api\/v1\/(issues\/[^/]+\/artifacts\/[^/]+|artifacts\/[^/]+)\/text$/;

test("a document's own text is read before the sign-in check has answered @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const { issueKey, path } = await seedDocument("notes.md", shortDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const asked = requestedPaths(page);
    // The sign-in check, held: every page waits on it (`app.tsx`'s `AuthGate` shows its
    // skeleton until it answers), so while it is out nothing of the shell, the issue page or
    // the margin has rendered at all.
    const signInHeld = Promise.withResolvers<void>();
    await page.route("**/auth/whoami", async (route) => {
      await signInHeld.promise;
      await route.continue();
    });
    await page.goto(path, { waitUntil: "commit" });

    // The document's own text, asked for from the address alone
    // (`web/src/features/doc/open-document-early.ts`), while the check is still out. The
    // address names the document, so the read is the slug route's; a Spec tab's address names
    // no slug and its text is read by the id the issue answers with.
    await expect
      .poll(() => asked.filter((what) => documentTextRoute.test(what)), {
        intervals: [100],
        message:
          "the document's text was never asked for while the sign-in check was out: the read " +
          "is waiting for the shell again",
        timeout: 60_000,
      })
      .toEqual([`/api/v1/issues/${issueKey}/artifacts/notes-md/text`]);
    // And the shell really has not rendered: the Inbox read it makes is still unmade.
    expect(asked).not.toContain("/api/v1/inbox");

    // Released, the page shows the document it already holds the text of.
    signInHeld.resolve();
    const firstBlock = page
      .locator(
        '[aria-label="Document text"] [data-block-id], [aria-label="Document editor"] [data-block-id]'
      )
      .first();
    await expect(firstBlock).toBeVisible({ timeout: 60_000 });
    await expect(firstBlock).toContainText("The lease ledger");
  } finally {
    await alice.close();
  }
});

test("a document's text is asked for before the entry's imports have evaluated @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  const { issueKey, path } = await seedDocument("notes.md", shortDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(path, { waitUntil: "commit" });
    const firstBlock = page
      .locator(
        '[aria-label="Document text"] [data-block-id], [aria-label="Document editor"] [data-block-id]'
      )
      .first();
    await expect(firstBlock).toBeVisible({ timeout: aDrawWithinMs });

    // What the browser itself timed: when the page asked for the document's text, and when the
    // entry's own module body began. Its first statement writes the
    // `dispatch:entry-imports-evaluated` mark (`web/src/main.tsx`), and a module evaluates every
    // static import of it before its own first statement runs, so a read that started before
    // that mark went out without waiting for any of them - which is what the page used to do
    // nothing but wait for, asking from the entry's body about 1.4 s into the load on a
    // throttled phone (`web/src/features/doc/open-document-early.ts`). Asking for it from
    // `web/index.html`'s head is what moves it in front of them. Both times are the browser's
    // own performance timeline, so nothing this row does sits between them.
    const timed = await page.evaluate((textPath: string) => {
      const resources = performance.getEntriesByType("resource") as PerformanceResourceTiming[];
      return {
        importsEvaluatedMs: performance.getEntriesByName("dispatch:entry-imports-evaluated")[0]
          ?.startTime,
        textAskedMs: resources.find((resource) => new URL(resource.name).pathname === textPath)
          ?.startTime,
      };
    }, `/api/v1/issues/${issueKey}/artifacts/notes-md/text`);

    expect(
      timed.importsEvaluatedMs,
      "the entry never marked itself: its first statement writes the " +
        "`dispatch:entry-imports-evaluated` mark (`web/src/main.tsx`)"
    ).toBeDefined();
    expect(timed.textAskedMs, "the document's text was never asked for at all").toBeDefined();
    const evaluatedMs = Math.round(timed.importsEvaluatedMs ?? Number.NaN);
    const askedMs = Math.round(timed.textAskedMs ?? Number.NaN);
    console.log(
      `R11 early read · the document's text was asked for at ${askedMs} ms, the entry's ` +
        `imports had evaluated at ${evaluatedMs} ms, ${evaluatedMs - askedMs} ms later.`
    );
    expect(
      timed.textAskedMs ?? Number.POSITIVE_INFINITY,
      "the document's text was asked for only after the entry's own first statement ran, so it " +
        "waited for every static import of the entry to evaluate before it went out " +
        "(`web/index.html`)"
    ).toBeLessThan(timed.importsEvaluatedMs ?? 0);
  } finally {
    await alice.close();
  }
});

/**
 * What the browser itself timed `page` asking for, by name: the Markdown engine's chunk, which is
 * the headless editor's entry (`@legion/proof-editor/headless`,
 * `packages/proof-editor/src/lib-headless.ts`) and which the build names after that module; the
 * parse worker's, the chunk the queue creates for a body over the threshold; and the document's
 * text. Nothing else on a document's page asks for either chunk.
 */
async function chunksAskedFor(
  page: Page
): Promise<{ engine: string[]; text: string[]; worker: string[] }> {
  return await page.evaluate(() => {
    const resources = performance.getEntriesByType("resource") as PerformanceResourceTiming[];
    const paths = resources.map((resource) => new URL(resource.name).pathname);
    return {
      engine: paths.filter((name) => /^\/assets\/lib-headless-[^/]*\.js$/.test(name)),
      worker: paths.filter((name) => /^\/assets\/markdown-parse\.worker-[^/]*\.js$/.test(name)),
      text: paths.filter((name) =>
        /^\/api\/v1\/(issues\/[^/]+\/artifacts\/[^/]+|artifacts\/[^/]+)\/text$/.test(name)
      ),
    };
  });
}

// The Markdown engine the page draws a document with - the headless Proof editor's chunk, 461 KB
// to download and compile, and the engine built over it - used to be loaded before the page had
// rendered anything: `openDocumentEarly` warmed it, and the shell warmed it again from its first
// commit. That buys a reader their first block where the page has to draw it. It buys nothing at
// all where the server drew the document's opening blocks into the page it sent
// (`web/src/features/doc/first-screen.ts`), because `DocumentPreview` adopts those blocks instead
// of parsing them - and on a phone at 4x CPU throttling that load is about a second and a half of
// the page's own thread, taken in front of the issue page's first render, which is the render
// that carries the document's surface, its socket and its editor (dispatch://INFRA-341, R11).
//
// So the engine left the boot task, and then the surface stopped asking for it at all while an
// editor is on its way (`DocumentPreview`'s `waitsForTheBuildMs`): on such a page the pass it
// would build the engine for is dropped at the build every time - the sync landed 0.5 to 1.1 s
// after the surface mounted in every run measured on this pod, and the parse worker answers the
// whole document about 16 s later - so the engine was a second headless Proof editor, 294 to
// 546 ms of the main thread, built for a draw that never reached the reader.
//
// This row reads the browser's own resource timing after the reader has an editor they can type
// in: neither the engine's chunk nor the parse worker's was ever asked for. Both are read by
// name (`chunksAskedFor`), so a name the build stopped giving a chunk would leave both lists
// empty with the engine built all the same; the row therefore opens the same document a second
// time with its socket held, where no build ever comes and the draw parses the whole document
// itself once its wait for one is over, and holds each name to at least one path there.
test("a page the server drew a document into builds no Markdown engine while its editor comes @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the engine's one reader here is the document's own preview, which is what a phone draws: " +
      "at a width that opens the margin, its cards draw Markdown of their own"
  );
  test.setTimeout(300_000);
  // Plain prose, well over the parse worker's threshold and well over the first screen the
  // server draws: nothing on this page but the document's own draw has any use for the engine,
  // so a chunk asked for is that draw asking for it. A decision block would bring its own card
  // and a diagram its own renderer, and neither is what this row is about.
  const prose = `# The lease ledger\n\n${"The paragraph a reader meets. ".repeat(8)}\n\n`.concat(
    Array.from({ length: 120 }, (_, index) => `## Section ${index}\n\nWhat it says.`).join("\n\n"),
    "\n"
  );
  const { path } = await seedDocument("ledger.md", prose);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(path, { waitUntil: "commit" });

    // The reader's own block: the editor's first, under a root that takes edits, which is what
    // the wait is for. Until it is there, nothing says the engine was skipped rather than
    // merely late.
    await expect(
      page.locator('.proof-editor[data-write-path="live"] [data-block-id]').first()
    ).toBeVisible({ timeout: aDrawWithinMs });

    const asked = await chunksAskedFor(page);

    expect(
      asked.text.length,
      "the document's text was never read, so this page never drew the document at all"
    ).toBeGreaterThan(0);
    expect(
      asked.engine,
      "the page built its own Markdown engine while the editor was on its way: a second " +
        "headless Proof editor for a draw the build replaces before it can paint " +
        "(`DocumentPreview`'s `waitsForTheBuildMs`)"
    ).toEqual([]);
    expect(
      asked.worker,
      "the page posted the whole document to the parse worker while the editor was on its way"
    ).toEqual([]);

    // The same document on a page whose socket never syncs: no build comes, so once its wait
    // for one is over the draw parses the whole document itself (`DocumentPreview`), which
    // builds the engine and posts the document to the worker. Each name above matches what
    // that page asked for, so the two empty lists are a page that asked for neither and not a
    // name that matches nothing. The draw waits `waitsForTheBuildMs`, 5 s, before it parses;
    // the whole row took 12.0 s on this pod with this page in it, so 30 s is the wait's own
    // five seconds and more than twice the rest of the row, and a hung parse fails at 30 s of a
    // shard step that has 540.
    const parsing = await alice.newPage();
    await documentTransport(parsing, { holding: true });
    await parsing.goto(path, { waitUntil: "commit" });
    await expect
      .poll(
        async () => {
          const { engine, worker } = await chunksAskedFor(parsing);
          return { engine: engine.length > 0, worker: worker.length > 0 };
        },
        {
          message:
            "a page whose editor never came never asked for a chunk by the engine's name and " +
            "the parse worker's, so neither name above is a name the build gives a chunk",
          timeout: 30_000,
        }
      )
      .toEqual({ engine: true, worker: true });
  } finally {
    await alice.close();
  }
});

test("a phone draws the margin's ask cards only once its reader opens the review panel @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "above the phone layout the margin is a column of its own, which is always on screen"
  );
  test.setTimeout(180_000);
  const { issueKey, path } = await seedDocument("notes.md", shortDocument);
  const question = "Which window should the lease ledger settle in?";
  await createAsk(
    issueKey,
    { options: [{ label: "The next window" }, { label: "The window after it" }], question },
    agent
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const asked = requestedPaths(page);
    await page.goto(path);
    await expect(documentEditor(page)).toContainText("The lease ledger", {
      timeout: aDrawWithinMs,
    });

    // The whole page is up, and the shut sheet holds no card: a card behind it is 64 px of
    // `Review (1)`, and the thread read each card makes is a read the reader never sees the
    // answer to (dispatch://INFRA-341, R11).
    const sheet = page.getByTestId("margin-sheet");
    await expect(sheet.locator("[data-margin-item]")).toHaveCount(0);
    expect(asked.filter((what) => /^\/api\/v1\/asks\/[^/]+$/.test(what))).toHaveLength(0);
    // The count beside `Review` is the margin's own asks read, which it makes either way.
    const review = page.getByRole("button", { name: /^Open review panel/ });
    await expect(review).toHaveAccessibleName("Open review panel (1 open ask)");

    await review.click();
    await expect(sheet.getByText(question)).toBeVisible();
    await expect(sheet.locator("[data-margin-item]")).toHaveCount(1);
    await expect
      .poll(() => asked.filter((what) => /^\/api\/v1\/asks\/[^/]+$/.test(what)), {
        intervals: [100],
        message: "the opened card never read its own thread",
        timeout: 30_000,
      })
      .toHaveLength(1);
  } finally {
    await alice.close();
  }
});

/** The build's own budget of recovery reloads, spent before the page loads, so the row below
 *  reads one page rather than the three the shell's inline script would reload through when it
 *  sees every script of the build fail (`web/index.html`, `CHUNK_RELOADS_STORAGE_KEY` in
 *  `web/src/features/shell/DeploymentResilience.tsx`). */
const CHUNK_RELOADS_SPENT = "dispatch.chunk-reloads";

// The reader meets their document in the page's own bytes. The Go server draws the document's
// opening blocks into the page it sends a reader whose session may read it
// (`internal/dispatch/pmdoc/first_screen.go`, dispatch://INFRA-341 R11, acceptance A11), so the
// first block is on the screen with none of the app downloaded, parsed or run: this row blocks
// every script the build serves and finds it there. What it proves is the one thing no
// measurement can - that the first block owes nothing at all to the page's own code.
test("draws the document's first block with the app's code blocked @phone", async ({ browser }) => {
  test.setTimeout(300_000);
  const { path } = await seedDocument("specs-blocked.md", smallDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.addInitScript(
      ([key, now]) => {
        window.sessionStorage.setItem(key as string, JSON.stringify([now, now, now]));
      },
      [CHUNK_RELOADS_SPENT, Date.now()]
    );
    await page.route("**/*.js", (route) => route.abort());
    await page.goto(path);

    const first = page.locator('[aria-label="Document text"] [data-block-id]').first();
    await expect(first).toBeVisible();
    await expect(first).toHaveText(documentTitle(documentDiagrams.small));
    // The app never ran: its own root is empty, and the editor it would build is nowhere.
    await expect(page.locator("#root")).toBeEmpty();
    await expect(page.getByRole("textbox", { name: "Document editor" })).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// --- The reads the first editable block needs none of ------------------------------------
//
// A document address spends its first seconds on one thing, and the three rows below are about
// everything else the page asks Dispatch for while it does. Measured on this pod, `iphone` at
// 4x CPU throttling, seven reads went out in front of the first block a reader can type in -
// 4,328 to 5,493 ms on a load whose preview mounted at 5,611 - and the block needs none of
// them: they fill the issue page's non-document panels (dispatch://INFRA-341, R11). They are
// made after the editor's first batch now (`web/src/features/doc/first-batch.ts`).
//
// Each row holds the document's socket (`documentTransport`), so the first batch happens when
// the row says so rather than when a loaded machine gets round to it, and reads the order off
// the browser's own resource timing (`watchTheReads`). None of them is a measurement: what
// they prove is which side of the batch a read falls on.
//
// Their document holds no decision block (`proseOnlyDocument`), because a decision block in
// the reader's own draw is a card they can answer before any editor exists and it reads the
// document's asks and its recipients to do so: a reader who has one on the screen is waiting
// for those two reads, so they go out there rather than at the batch, which is the rule
// working rather than failing.

/** The seven, by the path each goes out on. `/references` is the spec's `part_of` read, which
 *  the page makes only for a reader who has read the document before, so the row that reads it
 *  opens the spec twice. */
function readsTheFirstBlockNeedsNone(issueKey: string, project: string): readonly string[] {
  return [
    `/issues/${issueKey}/comments`,
    `/issues/${issueKey}/asks`,
    "/agents",
    "/settings/repo-projects",
    `/projects/${project}/architecture-sources`,
    `/issues/${issueKey}/subscribers`,
    "/references",
  ];
}

/** Every read the page made on one of `paths`, with its query, in the order the browser timed
 *  them: what a row names when it says a read went out it should not have. */
function readsAmong(reads: readonly PageRead[], paths: readonly string[]): PageRead[] {
  return reads.filter((read) => paths.includes(read.path.split("?")[0] ?? read.path));
}

/** When each of `paths` went out, in order, by path: every read the page made on that path
 *  with its query dropped, so one entry means one request. */
function whenEachWentOut(
  reads: readonly PageRead[],
  paths: readonly string[]
): Map<string, number[]> {
  const when = new Map<string, number[]>(paths.map((path) => [path, []]));
  for (const read of readsAmong(reads, paths)) {
    when.get(read.path.split("?")[0] ?? read.path)?.push(read.at);
  }
  return when;
}

/** Puts the order a row read on its annotations, which the report keeps with the result, and on
 *  stdout, which `--reporter line` prints as the row runs, exactly as `printMeasurement` does:
 *  a report says which read went out when rather than only that the row was green. */
function printTheOrder(
  what: string,
  firstBatchMs: number | undefined,
  when: Map<string, number[]>
): void {
  const line = `first batch ${
    firstBatchMs === undefined ? "never" : `${Math.round(firstBatchMs)} ms`
  }; ${[...when]
    .map(
      ([path, at]) => `${path} ${at.length === 0 ? "never" : `${at.map(Math.round).join(", ")} ms`}`
    )
    .join("; ")}`;
  test.info().annotations.push({ description: line, type: what });
  console.log(`${what} · ${line}`);
}

/** How long a row waits, with the socket held and the page settled, before it decides a read
 *  the page was going to make has not been made. The page's own draw of the document is on the
 *  screen well inside it, and every panel of the header has rendered.
 *
 *  Both this window and the editor's build after it have to finish inside the hold's own
 *  deadline, since a page held past that deadline releases the hold by design and the reads
 *  then go out with no batch behind them (`web/src/features/doc/first-batch.ts`,
 *  `LONGEST_HOLD_MS`, which the last of this file's chrome rows covers). The assertion below
 *  is what keeps the two in step when either number moves. */
const settledWithTheSocketHeldMs = 6_000;
if (settledWithTheSocketHeldMs * 2 >= LONGEST_HOLD_MS) {
  throw new Error(
    `the settled window (${settledWithTheSocketHeldMs} ms) leaves no room for the build inside the hold's deadline (${LONGEST_HOLD_MS} ms)`
  );
}

test("the reads the first editable block needs none of go out after the editor's first batch @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the seven were measured on a phone, where the margin's cards are a sheet the reader opens"
  );
  test.setTimeout(300_000);
  const { issueKey, path } = await seedDocument("specs-order.md", proseOnlyDocument);
  const paths = readsTheFirstBlockNeedsNone(issueKey, "BIG");

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    const transport = await documentTransport(page, { holding: true });
    await page.goto(path);

    // The page is up and the reader has the document's own draw. The header waits for the
    // batch too now (`web/src/features/doc/AfterTheFirstBatch.tsx`), so the reader reaches for
    // its box, which draws it: every panel the held reads belong to is then on the page, and
    // each of those reads still waits, because each has a gate of its own.
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeVisible({
      timeout: aDrawWithinMs,
    });
    await page.getByTestId("issue-header-box").click({ position: { x: 5, y: 5 } });
    await expect(page.getByRole("button", { name: /^Subscribers/ })).toBeVisible();
    await expect(page.locator('[data-testid="issue-components"]')).toBeVisible();
    // A read that never goes out cannot be awaited, so the row waits the settled window above:
    // a read any of these panels was going to make would have gone out inside it
    // (`settledWithTheSocketHeldMs`).
    await page.waitForTimeout(settledWithTheSocketHeldMs);

    const held = await readTheReads(page);
    expect(held.firstBatchMs).toBeUndefined();
    expect(
      readsAmong(held.reads, paths),
      "a read the first editable block needs none of went out before the editor's first batch"
    ).toEqual([]);

    // Released, the editor builds, and every one of them goes out behind its first batch.
    await transport.release();
    await expect(editorBeingBuilt(page)).toBeAttached({ timeout: aDrawWithinMs });
    await expect
      .poll(
        async () =>
          [...whenEachWentOut((await readTheReads(page)).reads, paths)].filter(
            ([, at]) => at.length === 0
          ).length,
        {
          message: "a read the page held behind the first batch never went out",
          timeout: aDrawWithinMs,
        }
      )
      .toBe(0);

    const after = await readTheReads(page);
    const when = whenEachWentOut(after.reads, paths);
    printTheOrder("reads after the first batch", after.firstBatchMs, when);
    const firstBatchMs = after.firstBatchMs ?? 0;
    expect(firstBatchMs).toBeGreaterThan(0);
    for (const [read, at] of when) {
      expect(at, `${read} never went out`).not.toHaveLength(0);
      for (const went of at) {
        expect(went, `${read} went out before the first batch`).toBeGreaterThan(firstBatchMs);
      }
    }
    // And none of them was made twice: one request per address, whichever panel wanted it.
    const addresses = readsAmong(after.reads, paths).map((read) => read.path);
    expect(addresses, "a read went out twice").toHaveLength(new Set(addresses).size);
  } finally {
    await alice.close();
  }
});

// The other half of the rule: a read is held from a reader who is not waiting for it, never
// from one who is. The Subscribers panel is the issue header's own, shut until the reader
// opens it, and opening it reads who is subscribed there and then - with the document's socket
// still held, so no first batch has happened and none is on its way.
test("a reader who opens a panel before the first batch has its read started then @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the seven were measured on a phone, where the margin's cards are a sheet the reader opens"
  );
  test.setTimeout(300_000);
  const { issueKey, path } = await seedDocument("specs-reached.md", proseOnlyDocument);
  const subscribers = `/issues/${issueKey}/subscribers`;
  const others = readsTheFirstBlockNeedsNone(issueKey, "BIG").filter(
    (read) => read !== subscribers
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    await documentTransport(page, { holding: true });
    await page.goto(path);

    // The header waits for the batch too, so the reader reaches for its box first; the panel
    // inside it is still shut, and its read still held.
    await page.getByTestId("issue-header-box").click({ position: { x: 5, y: 5 } });
    const toggle = page.getByRole("button", { name: /^Subscribers/ });
    await expect(toggle).toBeVisible({ timeout: aDrawWithinMs });
    // The settled window above: only time shows that the subscribers read nobody made is a read
    // nobody made (`settledWithTheSocketHeldMs`).
    await page.waitForTimeout(settledWithTheSocketHeldMs);
    expect((await readTheReads(page)).reads.filter((read) => read.path === subscribers)).toEqual(
      []
    );

    await toggle.click();
    await expect
      .poll(
        async () =>
          (await readTheReads(page)).reads.filter((read) => read.path === subscribers).length,
        { message: "the panel the reader opened never read its subscribers", timeout: 30_000 }
      )
      .toBe(1);

    // And it was the reader's own panel that did it, with no first batch and none of the other
    // held reads behind it.
    const reached = await readTheReads(page);
    printTheOrder(
      "the reader's own panel",
      reached.firstBatchMs,
      whenEachWentOut(reached.reads, [subscribers, ...others])
    );
    expect(reached.firstBatchMs).toBeUndefined();
    expect(
      [...whenEachWentOut(reached.reads, others)].filter(([, at]) => at.length > 0),
      "opening one panel started a read belonging to another"
    ).toEqual([]);
  } finally {
    await alice.close();
  }
});

// And a page with no document on it holds nothing: there is no first batch to wait for, so
// every one of those reads goes out as it did before INFRA-341. The issue's Conversation is
// that page - the same header, the same margin, no document surface - and the row reads it
// with no socket held at all, because there is no socket to hold.
test("a page with no document on it makes those reads as it always did @phone", async ({
  browser,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "iphone",
    "the seven were measured on a phone, where the margin's cards are a sheet the reader opens"
  );
  test.setTimeout(300_000);
  const { issueKey } = await seedDocument("specs-no-document.md", proseOnlyDocument);
  // Every one of the seven but the spec's `part_of` read, which belongs to the document this
  // page does not show.
  const paths = readsTheFirstBlockNeedsNone(issueKey, "BIG").filter(
    (read) => read !== "/references"
  );

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    await page.goto(`/issues/${issueKey}/conversation`);
    await expect(page.getByRole("button", { name: /^Subscribers/ })).toBeVisible({
      timeout: aDrawWithinMs,
    });

    await expect
      .poll(
        async () =>
          [...whenEachWentOut((await readTheReads(page)).reads, paths)].filter(
            ([, at]) => at.length === 0
          ).length,
        { message: "a read of a page with no document on it never went out", timeout: 60_000 }
      )
      .toBe(0);

    const made = await readTheReads(page);
    printTheOrder("no document on the page", made.firstBatchMs, whenEachWentOut(made.reads, paths));
    // Nothing was ever built, so nothing was ever waited for.
    expect(made.firstBatchMs).toBeUndefined();
    await expect(page.locator('.proof-editor[data-write-path="live"]')).toHaveCount(0);
    // And its chrome is the chrome it always had, drawn with nothing waited for: the header
    // the Subscribers control stands in, the tab strip itself rather than the box the page
    // keeps for it, and the shell's own inbox peek.
    await expect(page.getByRole("tab", { name: /^Spec$/ })).toBeVisible();
    await expect(page.getByTestId("issue-tabs-box")).toHaveCount(0);
    await expect(page.getByRole("button", { name: /^Peek at the inbox/ })).toBeVisible();
  } finally {
    await alice.close();
  }
});

// And the rule the three rows above are held to: a panel the page draws after the reader is in
// place moves what they are looking at by no pixels (`web/src/features/doc/readers-place.ts`).
// The panel here is the issue header's Subscribers list, whose read is one of the seven; the
// row holds that read at the server until the reader is deep in the document, so the panel
// lands under a page that has stopped moving rather than during the load, which is the state
// the rule is about. The browser's own scroll anchoring is off (`withoutScrollAnchoring`), as
// it is in Safari before 27, so what keeps the reader in place is the page's own doing.
test("a panel arriving while the reader is in a long document moves what they are looking at by no pixels @phone", async ({
  browser,
}) => {
  test.setTimeout(600_000);
  const { issueKey, path } = await seedDocument("specs-large.md", largeDocument);
  await subscribeSession(issueKey);
  // A section well down the document: the heading the reader stops at, and the words the row
  // measures. Far enough in that the whole header, panel and all, is off their screen.
  const landmarkText = sectionHeading(Math.floor(documentDiagrams.large / 2));

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const subscribers = Promise.withResolvers<void>();
    await page.route(`**/api/v1/issues/${issueKey}/subscribers`, async (route) => {
      await subscribers.promise;
      await route.continue();
    });
    await page.goto(path);
    await expect(documentEditor(page)).toBeVisible({ timeout: aLongDrawWithinMs });
    await withoutScrollAnchoring(page);

    // The reader opens the panel - its read is still held, so it is the empty frame of itself -
    // and reads their way down to the landmark, which is where they are when it lands.
    await page.getByRole("button", { name: "Subscribers", exact: true }).click();
    const landmark = documentEditor(page).getByRole("heading", { name: landmarkText });
    await landmark.scrollIntoViewIfNeeded();
    await settledScrollTop(page);
    await landmark.evaluate((heading) => heading.scrollIntoView({ block: "start" }));
    expect(await settledScrollTop(page)).toBeGreaterThan(10_000);

    // From here the row reads the landmark after every frame the browser paints - a task
    // scheduled from an animation frame runs once that frame is on the screen - so what it
    // records is what the reader saw, never a page the correction had not reached yet.
    await page.evaluate((text: string) => {
      const heading = [...document.querySelectorAll(".proof-editor h2")].find(
        (candidate) => candidate.textContent === text
      );
      window.__readersPlace = { grew: false, moved: 0, started: heading !== undefined };
      if (heading === undefined) return;
      const scrolled = document.scrollingElement?.scrollTop ?? 0;
      const started = heading.getBoundingClientRect().top;
      const sample = () => {
        const place = window.__readersPlace;
        if (place === undefined) return;
        const top = heading.getBoundingClientRect().top;
        const above = top + (document.scrollingElement?.scrollTop ?? 0) - scrolled;
        place.moved = Math.max(place.moved, Math.abs(top - started));
        if (Math.abs(above - started) > 0.5) place.grew = true;
        requestAnimationFrame(() => window.setTimeout(sample, 0));
      };
      sample();
    }, landmarkText);

    subscribers.resolve();
    await expect(page.getByRole("region", { name: "Subscribed agents" })).toContainText(
      "e2e-session-title",
      { timeout: 60_000 }
    );
    await expect
      .poll(async () => (await page.evaluate(() => window.__readersPlace))?.grew, {
        message: "the panel landing never changed how much page stands above the reader",
        timeout: 60_000,
      })
      .toBe(true);

    // The reader's words stay on the line they were on. Not to the exact float: a scroll offset
    // is a fraction of a pixel on a page this tall, so the correction lands within one, and a
    // pixel is the unit a reader has.
    const place = await page.evaluate(() => window.__readersPlace);
    expect(place?.started, "the row never found the heading it measures").toBe(true);
    expect(
      place?.moved ?? Number.NaN,
      "the panel moved the words the reader was looking at"
    ).toBeLessThan(1);
  } finally {
    await alice.close();
  }
});

// --- The chrome the page draws after that batch -------------------------------------------
//
// The reads above were half the rule; the React that draws their panels is the other half.
// Measured on this pod, the shell's own render and its commits cost the main thread 1,062 ms
// on the way to the first editable block of the 28-section document, and the issue page's
// header, tab strip and artifact chrome 522 ms of React inside a 1,065 ms window. The route
// has to render; the sidebar, the inbox peek, the live pill, the needs-you and replies counts
// and the page's own chrome do not, so each of them is drawn after the editor's first batch
// (dispatch://INFRA-341 R11, `web/src/features/doc/AfterTheFirstBatch.tsx`).
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
 *  on stdout, which `--reporter line` prints as the row runs, exactly as `printTheOrder` does:
 *  a report says which part was drawn when rather than only that the row was green. */
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
    // The settled window above: a first batch, or any chrome read behind it, would have gone
    // out inside it, so what the row reads next is what the held page never asked for
    // (`settledWithTheSocketHeldMs`).
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
    // The settled window above: with the socket held no batch ever comes, and this is how long
    // the row gives it before it reads that none did (`settledWithTheSocketHeldMs`).
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

// And the same reach taken the other way: by a reader with no pointer, whose Tab is the only
// way they have into a box the page is holding.
//
// The hand-over unmounts whatever they were standing on - the space and the chrome are
// different elements in the same slot, so React takes the space off the page in the commit the
// reach turns on, the reach inside it with it. Their focus fell to `<body>` then and their next
// Tab started again at the top of the page, which is the whole page walked a second time to get
// back to the chrome they had just asked for (dispatch://INFRA-341). `ChromeAfterTheFirstBatch`
// draws the reach as a control of its own standing first in the box, carries `tabIndex={-1}` on
// the box, and puts the focus back on the box in the commit the swap turned on.
// `IssuePage.test.tsx` holds that in happy-dom, which lays nothing out and moves no focus of its
// own; this row is the same hand-over under a real browser's own Tab, which is the only place
// the browser's focus and its tab order are the product's rather than a harness's.
//
// Both boxes are driven, because the finding had two halves. The header's is the one the reader
// meets first. The tab strip's keeps a row of `aria-hidden` spans and nothing else
// (`IssueTabsSkeleton`, `features/issue/issue-page-skeleton.tsx`), so without a reach of its own
// a reader tabbed from the header straight past the strip and into the document, with no way to
// ask for a tab at all.
//
// No `@phone`: what this reads is where a Tab leaves the focus, which is the same at either
// width, and the reach it drives is the page's own chrome at both. Untagged, it runs in the
// `chromium` project, and that is the full lane rather than every pull request: the
// pull-request lane names `chromium`, `iphone` and the two stubbed projects and greps `@pr`
// over them (`.github/workflows/dispatch-browser.yaml`'s shard job), so a row without that tag
// is run by the merge queue and the nightly and by a pull request carrying `full-ci`. It stays
// untagged deliberately. The tag is for a real-stack row whose failure a pull request has to
// see first, and the lane's real-stack half is held to 150 rows
// (`e2e/__tests__/pull-request-lane.test.ts`), of which 147 are taken at this head; this row
// seeds a document of its own and walks the page's whole tab order twice over, which is one
// of the three remaining slots spent on a focus order the merge queue catches
// before it reaches `main`.

/** The document's own draw, in every surface it can stand in: where a reader who tabbed past
 *  the chrome ends up, and the one place this row's second walk must not reach before the tab
 *  strip's reach. */
const theDocumentsOwnDraw = '#first-screen, [aria-label="Document text"], .proof-editor';

/** One element the page put the focus on: enough for a failure to name the stop, and the one
 *  thing the walks below are judged on - whether it stands in the document rather than on the
 *  chrome over it. */
interface Stop {
  /** Whether it stands inside the document's own draw (`theDocumentsOwnDraw`). */
  readonly inTheDocument: boolean;
  /** What a reader hears it as: its `aria-label`, else its own text, cut to one line. */
  readonly name: string;
  /** The element it is, lowercased, so a failure says `button` rather than only a name. */
  readonly tag: string;
}

/** Where the focus stands now, which is a different question from the one above: after the
 *  hand-over the stop the Tab landed on is gone, and this is what the reader was left with. */
interface Here extends Stop {
  /** The focus is on `<body>`, or the page holds none at all: what the hand-over used to
   *  leave behind, and the whole of what this row is about. */
  readonly body: boolean;
}

declare global {
  interface Window {
    /** Every element this page has put the focus on, oldest first, which `watchTheFocus`
     *  records and the two walks of the row below read. */
    __theFocusLanded?: Stop[];
  }
}

/**
 * Records every element this page moves the focus onto, from before its own scripts run.
 *
 * A read of `document.activeElement` after the press cannot see the reach at all. The reach is
 * a focus handler (`ChromeAfterTheFirstBatch`'s `onFocusCapture`) and React treats focus as a
 * discrete event, so the hand-over's commit runs inside the Tab itself: by the time a read
 * from here arrives, the button the Tab landed on has been unmounted and the box holds the
 * focus in its place. The page's own `focusin` records both, in order, which is what says the
 * reader's Tab reached the control and what the page then did with their focus.
 *
 * Install before the navigation; read with `theFocusTrail`.
 */
async function watchTheFocus(page: Page): Promise<void> {
  await page.addInitScript((draw: string) => {
    const landed: Stop[] = [];
    window.__theFocusLanded = landed;
    window.addEventListener(
      "focusin",
      (event) => {
        const target = event.target;
        if (!(target instanceof Element)) {
          return;
        }
        const text = (target.textContent ?? "").replace(/\s+/gu, " ").trim();
        landed.push({
          inTheDocument: target.closest(draw) !== null,
          name: (target.getAttribute("aria-label") ?? text).slice(0, 60),
          tag: target.tagName.toLowerCase(),
        });
      },
      true
    );
  }, theDocumentsOwnDraw);
}

/** Every element the page has put the focus on so far, oldest first. */
function theFocusTrail(page: Page): Promise<Stop[]> {
  return page.evaluate(() => window.__theFocusLanded ?? []);
}

/** Where the focus stands now. `toBeFocused` answers one locator; a walk has to ask the page
 *  what it is standing on, and a row that reads `<body>` has to be able to say so. */
function whereTheFocusIs(page: Page): Promise<Here> {
  return page.evaluate((draw: string) => {
    const active = document.activeElement;
    if (active === null) {
      return { body: true, inTheDocument: false, name: "", tag: "none" };
    }
    const text = (active.textContent ?? "").replace(/\s+/gu, " ").trim();
    return {
      body: active === document.body,
      inTheDocument: active.closest(draw) !== null,
      name: (active.getAttribute("aria-label") ?? text).slice(0, 60),
      tag: active.tagName.toLowerCase(),
    };
  }, theDocumentsOwnDraw);
}

/** Tabs forward until the focus has landed on a stop `arrived` accepts, and answers every stop
 *  it landed on from this walk's first press, in order. Bounded, because a page whose reach is
 *  missing is one this would tab around forever - the bound is far above the stops either walk
 *  of this row takes, and a walk that spends it answers the whole trail, which is what says
 *  where the reader went instead. */
async function tabbingTo(
  page: Page,
  arrived: (stop: Stop) => boolean,
  stops = 60
): Promise<Stop[]> {
  const before = (await theFocusTrail(page)).length;
  for (let press = 0; press < stops; press += 1) {
    await page.keyboard.press("Tab");
    const walked = (await theFocusTrail(page)).slice(before);
    if (walked.some(arrived)) {
      return walked;
    }
  }
  return (await theFocusTrail(page)).slice(before);
}

/** One stop as an assertion names it: the element and what a reader hears it as. A stop is
 *  read as this one string rather than matched field by field, so a walk that ends on the
 *  wrong stop - or on none at all - fails saying what the reader did reach, with the message
 *  the row gave and the trail in it. */
function theStop(stop: Stop): string {
  return `<${stop.tag}> ${stop.name === "" ? "(no name)" : stop.name}`;
}

/** A walk as a failure prints it: one stop a line, in the order the reader met them. */
function theTrail(trail: readonly Stop[]): string {
  return trail.map((stop, index) => `${index}. ${theStop(stop)}`).join("\n");
}

test("a reader with no pointer tabs into each held box and goes on into the chrome it hands them", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  const { path } = await seedDocument("specs-chrome-tabbed.md", proseOnlyDocument);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await watchTheReads(page);
    await watchTheFocus(page);
    await documentTransport(page, { holding: true });
    await page.goto(path);

    // The page as this reader meets it: the document's own text, the two boxes the chrome will
    // stand in, and nothing of the chrome in either of them.
    await expect(page.locator('[aria-label="Document text"] [data-block-id]').first()).toBeVisible({
      timeout: aDrawWithinMs,
    });
    await expect(page.getByTestId("issue-header-box")).toBeAttached();
    await expect(page.getByTestId("issue-tabs-box")).toBeAttached();
    await expect(page.getByRole("tab", { name: /^Spec$/ })).toHaveCount(0);
    expect((await readTheReads(page)).firstBatchMs).toBeUndefined();

    // The reader starts where a page just opened leaves them, and their first Tab is kept as a
    // handle: it is the stop the page's own tab order starts at, which a reader walked round to
    // the long way whenever this hand-over lost their focus, and which the Tab after the
    // hand-over must therefore not be.
    expect((await whereTheFocusIs(page)).body, "the page did not open with the focus on it").toBe(
      true
    );
    await page.keyboard.press("Tab");
    const theTopOfThePage = await page.evaluateHandle(() => document.activeElement);
    expect((await whereTheFocusIs(page)).body, "the page's first Tab reached nothing").toBe(false);

    // Tabbing on reaches the header's box while its space is still held: the reach is a
    // control of its own standing first in the box, and it says what it brings. It is read
    // from the page's own focus events rather than from `document.activeElement`, because the
    // hand-over has already taken it off the page by the time a read from here lands
    // (`watchTheFocus`).
    const toTheHeader = await tabbingTo(page, (stop) => stop.name === "Show the issue header");
    expect(
      toTheHeader.map(theStop),
      `no Tab reached the held header's own reach. The trail:\n${theTrail(toTheHeader)}`
    ).toContain("<button> Show the issue header");

    // That Tab is the reader reaching for the chrome, and they get it then.
    const header = page.getByTestId("issue-header");
    await expect(header).toBeVisible({ timeout: 30_000 });
    await expect(page.getByRole("combobox", { name: "Status" })).toBeVisible();
    // And it was their own Tab that brought it rather than the hold letting go: a deadline that
    // fired would release every box at once, and the tab strip is still a skeleton.
    await expect(page.getByTestId("issue-tabs-box")).toBeAttached();
    await expect(page.getByRole("tab", { name: /^Spec$/ })).toHaveCount(0);

    // The commit that brought it took the control they were standing on off the page, and their
    // focus did not fall to `<body>` with it: the box they reached through is what holds them.
    expect(
      (await whereTheFocusIs(page)).body,
      "the hand-over dropped the reader's focus to <body>"
    ).toBe(false);
    await expect(header.locator("..")).toBeFocused();

    // So their next Tab goes on into the header they asked for, rather than starting the page
    // again at the stop their first Tab took.
    await page.keyboard.press("Tab");
    const intoTheHeader = await whereTheFocusIs(page);
    expect(
      await page.evaluate((top) => document.activeElement === top, theTopOfThePage),
      "the Tab after the hand-over started again at the top of the page"
    ).toBe(false);
    expect(
      await header.evaluate((node) => node.contains(document.activeElement)),
      `the Tab after the hand-over left the header: <${intoTheHeader.tag}> ${intoTheHeader.name}`
    ).toBe(true);

    // The other half: tabbing on out of the header meets the strip's own box, whose space
    // holds nothing else a reader can reach, before it meets the document under it. The walk
    // stops at whichever of the two comes first, and the first one is the reach.
    const theReachOrTheDocument = (stop: Stop) =>
      stop.name === "Show the issue tabs" || stop.inTheDocument;
    const toTheStrip = await tabbingTo(page, theReachOrTheDocument);
    const theFirstOfTheTwo = toTheStrip.find(theReachOrTheDocument);
    expect(
      theFirstOfTheTwo === undefined ? "nothing at all" : theStop(theFirstOfTheTwo),
      `the reader tabbed out of the header without meeting the tab strip's reach. The trail:\n${theTrail(toTheStrip)}`
    ).toBe("<button> Show the issue tabs");

    // And the strip is handed over the same way, with their place kept the same way.
    const tabs = page.getByTestId("issue-tabs");
    await expect(tabs).toBeVisible({ timeout: 30_000 });
    await expect(page.getByRole("tab", { name: /^Spec$/ })).toBeVisible();
    expect(
      (await whereTheFocusIs(page)).body,
      "the strip's hand-over dropped the reader's focus to <body>"
    ).toBe(false);
    await expect(tabs.locator("..")).toBeFocused();

    await page.keyboard.press("Tab");
    const intoTheStrip = await whereTheFocusIs(page);
    expect(
      await tabs.evaluate((node) => node.contains(document.activeElement)),
      `the Tab after the strip's hand-over left the strip: <${intoTheStrip.tag}> ${intoTheStrip.name}`
    ).toBe(true);
    // The whole of it was the reader's doing: no first batch ever landed behind them.
    expect((await readTheReads(page)).firstBatchMs).toBeUndefined();
  } finally {
    await alice.close();
  }
});
