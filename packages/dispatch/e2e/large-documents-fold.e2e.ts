// Where the chrome over a large document leaves the first screen the server drew.
//
// The rows of `large-documents-first-screen.the-chrome.e2e.ts` are about when each part of the
// page is drawn. This one is about where those parts leave the document. The server's draw is the
// reader's first screen (`internal/dispatch/pmdoc/first_screen.go`): it stands in the page's own
// bytes seconds before the page's code has run, which is the whole of R11's point, and a draw
// the chrome above it has pushed past the bottom of the viewport is a draw that reader never
// sees. What stands over it is the shell's own header, the issue's header, the tab strip and
// the document's own header, and the document's header is the one that grows: at 390 px its
// controls wrap, and each line they wrap to is 44 px of touch-sized control
// (`web/src/styles.css`) and the gap above it.
//
// Measured at this branch's merge head, with `Correct title` and `Remove` wrapping onto a line
// of their own: `window.innerHeight` 664, the document's header 224 px, and the block the
// server drew first standing at 646..672 - its first line of text cut by the fold, with 18 px
// of a 26 px block on the screen and nothing of the document readable. With them packed into
// the header's own row (`web/src/features/artifacts/ArtifactHeader.tsx`) the header is 168 px
// and the block stands at 590..616, whole and above the fold.
//
// The row runs at both widths because the claim is the reader's at both: a document opened is a
// document on the screen. The phone is where it failed, which is why it carries `@phone`.
//
// It seeds the one short section `shortDocument` spells, because where the first block stands
// is the same over any length of document. The other files of this family, and what each
// holds, are listed once, at the head of `large-documents-rows.ts`, which holds what they
// share.
import { type BrowserContext, expect, type Page, test } from "@playwright/test";

import { documentEditor } from "./editor";
import { aDrawWithinMs, seedDocument, shortDocument } from "./large-documents-rows";
import { asUser } from "./users";

// The row's two waits, for the document's header and for its editor to hold the document, are
// bounded by `aDrawWithinMs` (`large-documents-rows.ts`, which says the leg the row runs in, the
// rule a bound is held to and what those two waits cost). The row costs 4.4 s passing, and one
// that never draws either fails at about 2 s of seeding and its own read plus 20 s, then fails
// its retry the same way: about 40 s more on one worker.

/** The id of the block the server drew first, read from the bytes the server sends rather than
 *  from the page built out of them: the page adopts the server's element about a second in and
 *  the editor swaps its own copy of that block in under the reader later
 *  (`web/src/features/doc/first-screen.ts`), so the server's own answer is the one reading of
 *  it that no code of the page can have moved. The row measures the block of this id, on
 *  whichever of the three surfaces is holding it when it measures. */
async function theBlockTheServerDrewFirst(alice: BrowserContext, path: string): Promise<string> {
  const sent = await alice.request.get(path);
  expect(sent.ok(), `the server answered the document's address with ${sent.status()}`).toBe(true);
  const drawn = /data-first-screen-address="[^"]*"[\s\S]*?data-block-id="([^"]*)"/u.exec(
    await sent.text()
  )?.[1];
  expect(drawn, "the page the server sent carries no drawn first screen").toBeDefined();
  return drawn ?? "";
}

/** What the opened page left the reader, read in one evaluate so every number is one layout. */
interface TheOpenedPage {
  /** The bottom of the block the server drew first, from the top of the viewport. */
  readonly blockBottom: number;
  /** Which of the document's surfaces is holding that block, for a failure to name. */
  readonly blockOn: string;
  /** The top of that block, from the top of the viewport: the number this row is about. */
  readonly blockTop: number;
  /** How tall the document's own header is, which is the box that grows with its controls. */
  readonly headerHeight: number;
  readonly innerHeight: number;
  /** How far the page stands scrolled, which is nothing: the reader has only opened it. */
  readonly scrolled: number;
}

/** Where the opened page leaves the block of `blockId`, with the fold it is measured against. */
function theOpenedPage(page: Page, blockId: string): Promise<TheOpenedPage> {
  return page.evaluate((id: string) => {
    const block = document.querySelector(`[data-block-id="${CSS.escape(id)}"]`);
    const box = block?.getBoundingClientRect();
    const header = document
      .querySelector('[data-testid="artifact-header"]')
      ?.getBoundingClientRect();
    return {
      blockBottom: Math.round(box?.bottom ?? Number.NaN),
      blockOn:
        block === null
          ? "no surface at all"
          : block.closest(".proof-editor") !== null
            ? "the editor"
            : block.closest("#first-screen") !== null
              ? "the server's own element"
              : block.closest('[aria-label="Document text"]') !== null
                ? "the page's own draw"
                : "nothing of the document",
      blockTop: Math.round(box?.top ?? Number.NaN),
      headerHeight: Math.round(header?.height ?? Number.NaN),
      innerHeight: window.innerHeight,
      scrolled: Math.round(document.scrollingElement?.scrollTop ?? 0),
    };
  }, blockId);
}

test("a reader who opens a document has its first block inside the viewport, unscrolled @phone", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  const { path } = await seedDocument("notes.md", shortDocument);

  const alice = await asUser(browser, "alice");
  try {
    const drewFirst = await theBlockTheServerDrewFirst(alice, path);
    const page = await alice.newPage();
    await page.goto(path);

    // The page as the reader has it a moment later: the document's own header drawn, with both
    // of its way-back controls on it, and the editor built. That is the page at its tallest
    // above the document - nothing else of the chrome is still to arrive - and the place it
    // leaves the first block is the place the reader is left in.
    const header = page.getByTestId("artifact-header");
    await expect(header).toBeVisible({ timeout: aDrawWithinMs });
    await expect(header.getByRole("button", { exact: true, name: "Correct title" })).toBeVisible();
    await expect(header.getByRole("button", { exact: true, name: "Remove" })).toBeVisible();
    await expect(documentEditor(page)).toContainText("The lease ledger", {
      timeout: aDrawWithinMs,
    });

    const opened = await theOpenedPage(page, drewFirst);
    const reading =
      `the viewport is ${opened.innerHeight} px, the document's header ${opened.headerHeight} px, ` +
      `and the block the server drew first stands at ${opened.blockTop}..${opened.blockBottom} px ` +
      `on ${opened.blockOn}`;
    test.info().annotations.push({ description: reading, type: "the document a reader opens" });
    console.log(`the document a reader opens · ${reading}`);

    // Nothing scrolled it there: the reader has pressed nothing and the row has scrolled
    // nothing, so what the numbers above are of is the page as it opened.
    expect(opened.scrolled, "the page was scrolled before the row read it").toBe(0);
    expect(
      opened.blockTop,
      `the document's first block is above the top of the viewport: ${reading}`
    ).toBeGreaterThanOrEqual(0);
    expect(
      opened.blockBottom,
      "a reader who opened the document was left none of it to read: the chrome over the " +
        "document is so tall that the fold cuts the first block of the server's own drawn " +
        `first screen. ${reading}`
    ).toBeLessThanOrEqual(opened.innerHeight);
  } finally {
    await alice.close();
  }
});
