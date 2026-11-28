/** The reporter e2e/__tests__/shard-split.test.ts lists a whole selection through: it writes every
 *  unit of the listing as `unitsOf` of e2e/shard-reporter.ts builds it - its key, and its rows as
 *  `rowOf` builds each: its project, its file, its titles below the file joined by
 *  `titleSeparator`, and whether it runs - to the file SHARD_UNITS_OUTPUT names, as one JSON array
 *  in the order they were built. That reporter's `preprocess` places the units `unitsOf` builds,
 *  so this writes what `preprocess` places, read by the same code. */
import { writeFileSync } from "node:fs";
import type { FullConfig, Reporter, Suite } from "@playwright/test/reporter";

import { unitsOf } from "../../shard-reporter";

export default class ShardUnitsReporter implements Reporter {
  printsToStdio(): boolean {
    return false;
  }

  onBegin(_config: FullConfig, suite: Suite): void {
    const output = process.env.SHARD_UNITS_OUTPUT;
    if (!output) {
      throw new Error("shard-units.reporter.ts needs SHARD_UNITS_OUTPUT to name its file");
    }
    writeFileSync(output, JSON.stringify(unitsOf(suite).map(({ key, rows }) => ({ key, rows }))));
  }
}
