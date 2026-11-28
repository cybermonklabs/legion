/** The row needs the real server to index a copy with 9,252 comments written into Postgres
 *  and answer the API reads it times through the browser's session. */

import { runScaleCopy } from "./issue-page-n-scale";
import { test } from "./test";

// c-scale-whole (INFRA-663): 9,252 comments with 60 replies on one, 25 documents, 6 questions.
// SLOW(>=2s): INFRA-663's copy written and the block index waited for, seven cold loads of the
// page, and each distinct read of those loads sent 35 times.
test("the page keeps its load bound on the largest issues under each control of the row: INFRA-663 @phone", async ({
  browser,
}) => {
  // The comments seed and seven cold loads take the longest. The probe measured up to
  // 33 s to draw the view controls. Leave room for the timed reads within the shard limit.
  test.setTimeout(720_000);
  await runScaleCopy(browser, "INFRA-663");
});
