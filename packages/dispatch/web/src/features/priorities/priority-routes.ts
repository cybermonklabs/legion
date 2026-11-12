/** Where a priority is read. A priority belongs to no project, so its addresses carry none:
 *  `/priorities` lists every one of them and `/priorities/<id>` is one priority's own page.
 *  Both are registered in `app.tsx`, and every link to a priority is written from here, so a
 *  reader's address and the route that answers it cannot drift apart. */
export const PRIORITIES_PATH = "/priorities";

/** The address of one priority's page. The id is a slug (`linkage-closure`), but it is encoded
 *  all the same: nothing of the web app checks the grammar the server declares for it. */
export const priorityPath = (id: string): string => `${PRIORITIES_PATH}/${encodeURIComponent(id)}`;
