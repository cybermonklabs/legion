/** The row waits for the real block index to drain on a copy with documents, files, comments,
 *  questions and messages, then measures the server's API reads through the browser's session. */

import { runScaleCopy } from "./issue-page-n-scale";
import { test } from "./test";

// c-scale-whole (INFRA-1098): 99th-percentile mix — 22 documents of 3,409 blocks, 26 files,
// 100 comments, 25 questions, 2 messages.
// SLOW(>=2s): INFRA-1098's copy written and the block index waited for, seven cold loads of the
// page, and each distinct read of those loads sent 35 times.
test("the page keeps its load bound on the largest issues under each control of the row: INFRA-1098 @phone", async ({
  browser,
}) => {
  // Allow for seeding, the block index drain, and timing the reads of every control.
  test.setTimeout(480_000);
  await runScaleCopy(browser, "INFRA-1098");
});
