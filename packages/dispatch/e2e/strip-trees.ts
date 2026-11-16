import type { BrowserContext, Page } from "@playwright/test";

/** Serves every `/api/v1/` answer of `target` with its display trees taken out, which is what a
 *  pod of the build before this one answers while a deploy rolls: the page then holds each
 *  stored text's markdown alone and parses it itself (`MarkdownBody`'s fallback). The rename
 *  keeps each member rather than deleting it, so an answer's shape is otherwise untouched and
 *  only the name a reader looks for is gone.
 *
 *  The two transport headers go with it: a route that fulfils a decoded body under the
 *  `Content-Encoding` and `Content-Length` of the compressed one it fetched describes a body it
 *  is not sending, and the browser refuses it.
 *
 *  `target` is a context, where every page of the run reads the stripped answers, or one page. */
export async function serveWithoutDisplayTrees(target: BrowserContext | Page): Promise<void> {
  await target.route(/\/api\/v1\//, async (route) => {
    const response = await route.fetch();
    const body = (await response.text()).replaceAll(/"[a-z_]+_tree":/g, '"dropped_tree":');
    const headers = { ...response.headers() };
    delete headers["content-encoding"];
    delete headers["content-length"];
    await route.fulfill({ body, headers, status: response.status() });
  });
}
