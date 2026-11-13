// What a reader of a large document is given before their editor, and what the page holds back
// until they have one.
//
// The document's own reads are started from the address, the first of them before any of the
// page's own code has evaluated, so they wait for nothing the entry chunk or the issue page
// does; the first block is in the page's own bytes, drawn there by the Go server
// (`internal/dispatch/pmdoc/first_screen.go`), so it owes nothing at all to the page's own
// code; the margin beside the document draws no card a reader cannot see; and the reads that
// fill the rest of the issue page go out after the editor's first batch
// (`web/src/features/doc/first-batch.ts`). None of these rows is timed: each reads what the page
// asked for, what it drew and which side of the first batch either fell on, never how long any
// of it took. The numbers that load is mostly made of are in `large-documents-measured.e2e.ts`.
// The chrome the page draws after that same batch (`web/src/features/doc/AfterTheFirstBatch.tsx`)
// is in `large-documents-first-screen.the-chrome.e2e.ts`, and a reader with no pointer reaching
// for it is in `large-documents-first-screen.the-keyboard.e2e.ts`: rows of this file until it
// reached 1,391 lines, now in files whose names list right after it, so every project runs them
// in the order it did.
//
// Their documents are short ones, because an order is the same over any length of document. The
// one exception is the row that reads the reader's place half-way down the larger document of
// `large-documents.ts`: a panel landing under a reader who has placed themselves needs a page
// tall enough for them to have read their way into it.
//
// The numbers R11's targets are read from are in `large-documents-measured.e2e.ts`, and the
// rows beside them that need a build of the larger document to last are in
// `large-documents-build-serves.e2e.ts`, `large-documents-build-serves.the-decision.e2e.ts`,
// `large-documents-desktop-build.e2e.ts` and `large-documents-long-build.e2e.ts`; what stands
// on the reader's screen while the editor is built is in `large-documents-build.e2e.ts`; what a
// reader does with a document whose editor is still being built is in
// `large-documents-reader.e2e.ts`; where the chrome draws leaves the server's first block, a
// place rather than an order, is in `large-documents-fold.e2e.ts`. What the eleven files share
// is `large-documents-rows.ts`.
import { expect, type Page, test } from "@playwright/test";

import { subscribeSession } from "./agents";
import { createAsk } from "./api";
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
  readTheReads,
  settledWithTheSocketHeldMs,
  watchTheReads,
} from "./large-documents-after-the-batch";
import {
  aDrawWithinMs,
  agent,
  aLongDrawWithinMs,
  seedDocument,
  shortDocument,
} from "./large-documents-rows";
import { settledScrollTop, withoutScrollAnchoring } from "./mermaid-helpers";
import { asUser } from "./users";

// Every wait of this file for something the page draws is bounded by `aDrawWithinMs` or
// `aLongDrawWithinMs` (`large-documents-rows.ts`, which says the leg these rows run in, the rule
// a bound is held to and what the waits it bounds cost). The longest a row of this file runs
// before it fails is the long-document row's: the 9.6 s it spends before its editor wait and
// 40 s, 49.6 s; twice that, less the 22.6 s the row costs passing, is 76.6 s more on one worker.

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
    // A read that never goes out cannot be awaited, so the row waits the settled window: a read
    // any of these panels was going to make would have gone out inside it
    // (`settledWithTheSocketHeldMs`, `large-documents-after-the-batch.ts`).
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
    // The settled window: only time shows that the subscribers read nobody made is a read
    // nobody made (`settledWithTheSocketHeldMs`, `large-documents-after-the-batch.ts`).
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
