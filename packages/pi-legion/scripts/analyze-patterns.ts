/**
 * `bun run analyze:patterns`: recheck's reading of each pattern `cleanDetail` runs
 * (`CLEAN_DETAIL_PATTERNS`), written to the record `capability-report-patterns.test.ts` holds each
 * slow analysis to. Run it after changing one of those patterns, and commit the record it writes.
 * Four rules' analyses have no verdict in the time each is given; the test names them.
 */
import { rewritePatternRecord } from "@legion/envoy-client/test/pattern-analysis";
import { CLEAN_DETAIL_PATTERNS } from "../src/capability-report";

await rewritePatternRecord(
  new URL("../src/capability-report-patterns.recheck.json", import.meta.url),
  CLEAN_DETAIL_PATTERNS,
  "Written by `bun run analyze:patterns` in packages/pi-legion (scripts/analyze-patterns.ts): " +
    "recheck's reading of each pattern cleanDetail runs whose analysis takes more than a moment, " +
    "which capability-report-patterns.test.ts holds to the pattern's source."
);
