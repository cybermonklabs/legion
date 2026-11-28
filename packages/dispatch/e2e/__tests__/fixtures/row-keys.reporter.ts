/** The reporter e2e/__tests__/shard-split.test.ts lists `row-keys.config.ts` through: it writes
 *  every row of the listing as e2e/shard-reporter.ts keys it - project, file, and its titles below
 *  the file (`titlesBelowFile`) joined by `titleSeparator` - to the file ROW_KEYS_OUTPUT names, as
 *  one JSON array. The project and the file are read as that reporter's `preprocess` reads them. */
import { writeFileSync } from "node:fs";
import { sep } from "node:path";
import type { FullConfig, Reporter, Suite } from "@playwright/test/reporter";

import { titleSeparator } from "../../configurations";
import { titlesBelowFile } from "../../shard-reporter";

export default class RowKeysReporter implements Reporter {
  printsToStdio(): boolean {
    return false;
  }

  onBegin(_config: FullConfig, suite: Suite): void {
    const output = process.env.ROW_KEYS_OUTPUT;
    if (!output) throw new Error("row-keys.reporter.ts needs ROW_KEYS_OUTPUT to name its file");
    const rows = suite.suites.flatMap((projectSuite) => {
      const project = projectSuite.project()?.name ?? projectSuite.title;
      return projectSuite.suites.flatMap((fileSuite) => {
        const file = fileSuite.title.split(sep).join("/");
        return fileSuite.allTests().map((test) => ({
          file,
          project,
          title: titlesBelowFile(test).join(titleSeparator),
        }));
      });
    });
    writeFileSync(output, JSON.stringify(rows));
  }
}
