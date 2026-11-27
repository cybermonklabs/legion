/** The row waits for the server to index a copy with 45,738 document blocks and times the
 *  real API reads from its cold page loads. A recording would bypass indexing and server work. */

import { runScaleCopy } from "./issue-page-n-scale";
import { test } from "./test";

// c-scale-whole (INFRA-3149): 69 documents of 45,738 blocks (5,600 in the largest), 39 files,
// 113 comments, 6 questions.
// SLOW(>=2s): INFRA-3149's copy written and the block index waited for, seven cold loads of the
// page, and each distinct read of those loads sent 35 times.
test("the page keeps its load bound on the largest issues under each control of the row: INFRA-3149 @phone", async ({
  browser,
}) => {
  // Allow for writing the documents and files, draining the block index, and timing the reads.
  test.setTimeout(540_000);
  await runScaleCopy(browser, "INFRA-3149");
});
