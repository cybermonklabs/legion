/** The reporter e2e/__tests__/shard-split.test.ts lists `row-keys.config.ts` through: it writes
 *  every row of the listing as `rowOf` of e2e/shard-reporter.ts builds it - its project, its file,
 *  its titles below the file joined by `titleSeparator`, and whether it runs - to the file
 *  ROW_KEYS_OUTPUT names, as one JSON array. That reporter's `preprocess` places every row as
 *  `rowOf` builds it, so this writes what `preprocess` places by, read by the same code. */
import { writeFileSync } from "node:fs";
import type { FullConfig, Reporter, Suite } from "@playwright/test/reporter";

import { rowOf } from "../../shard-reporter";

export default class RowKeysReporter implements Reporter {
  printsToStdio(): boolean {
    return false;
  }

  onBegin(_config: FullConfig, suite: Suite): void {
    const output = process.env.ROW_KEYS_OUTPUT;
    if (!output) throw new Error("row-keys.reporter.ts needs ROW_KEYS_OUTPUT to name its file");
    const rows = suite.suites.flatMap((projectSuite) =>
      projectSuite.suites.flatMap((fileSuite) =>
        fileSuite.allTests().map((test) => rowOf(projectSuite, fileSuite, test))
      )
    );
    writeFileSync(output, JSON.stringify(rows));
  }
}
