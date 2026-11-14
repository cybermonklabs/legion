import type { BrowserContext, Page } from "@playwright/test";

/** The element the server draws a document's opening blocks in, as `firstScreenElement` writes
 *  it (`internal/dispatch/routes/first_screen.go`). Matched by the attribute that names the
 *  address rather than by the whole tag, so a tag that gains an attribute is still found. */
const SERVERS_DRAW = /<div[^>]*\sdata-first-screen-address="[^"]*"[^>]*>/;

/** `<div` or `</div>`, for the walk that finds where the drawn element closes: the blocks
 *  inside it hold elements of their own, so the first `</div>` after it is not its own. */
const DIV_TAG = /<div\b|<\/div>/g;

/** `page` with the blocks the server drew taken out of it, which leaves the page's own
 *  `#first-screen` element as empty as the build ships it. The element is found by its address
 *  attribute and closed by a walk that counts the `div`s inside it; a page carrying no draw is
 *  answered unchanged. */
function withoutTheServersDraw(page: string): string {
  const opened = SERVERS_DRAW.exec(page);
  if (opened === null) {
    return page;
  }
  DIV_TAG.lastIndex = opened.index + opened[0].length;
  let depth = 1;
  let tag = DIV_TAG.exec(page);
  while (tag !== null) {
    depth += tag[0] === "</div>" ? -1 : 1;
    if (depth === 0) {
      return page.slice(0, opened.index) + page.slice(tag.index + tag[0].length);
    }
    tag = DIV_TAG.exec(page);
  }
  return page;
}

/** Serves every answer of `target` with the reading Go made of each stored text taken out of
 *  it, which is what a pod of the build before this one answers while a deploy rolls: the page
 *  then holds each stored text's markdown alone and parses it itself (`MarkdownBody`'s
 *  fallback).
 *
 *  Go sends that reading two ways, and both go. In an `/api/v1/` answer it is the `*_tree`
 *  member beside each text; the rename keeps each member rather than deleting it, so an
 *  answer's shape is otherwise untouched and only the name a reader looks for is gone. In the
 *  page itself it is the document's opening blocks, drawn into the bytes the server sends for a
 *  document's address (`internal/dispatch/routes/first_screen.go`): a page carrying those needs
 *  no reading of its own for the screenful a reader meets first, and leaving them in would
 *  serve a reading to the one surface this exists to take every reading from.
 *
 *  The two transport headers go with both: a route that fulfils a decoded body under the
 *  `Content-Encoding` and `Content-Length` of the compressed one it fetched describes a body it
 *  is not sending, and the browser refuses it.
 *
 *  `target` is a context, where every page of the run reads the stripped answers, or one page. */
export async function serveWithoutDisplayTrees(target: BrowserContext | Page): Promise<void> {
  // One route for both, because only a document's own address carries a drawn page and that
  // address is spelled like the reads under it: `/issues/…`. A request that is no page is an
  // answer of the API, and is stripped as one.
  await target.route(/\/(api\/v1|issues)\//, async (route) => {
    const response = await route.fetch();
    const served = await response.text();
    const body =
      route.request().resourceType() === "document"
        ? withoutTheServersDraw(served)
        : served.replaceAll(/"[a-z_]+_tree":/g, '"dropped_tree":');
    const headers = { ...response.headers() };
    delete headers["content-encoding"];
    delete headers["content-length"];
    await route.fulfill({ body, headers, status: response.status() });
  });
}
