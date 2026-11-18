/**
 * `bun run analyze:patterns`: recheck's reading of each of `excerpt`'s patterns
 * (`EXCERPT_PATTERNS`), written to the record `excerpt-patterns.test.ts` holds each slow analysis
 * to. Run it after changing one of those patterns, and commit the record it writes.
 */
import { EXCERPT_PATTERNS } from "../src/dispatch-http";
import { rewritePatternRecord } from "./pattern-analysis";

await rewritePatternRecord(
  new URL("../src/__tests__/excerpt-patterns.recheck.json", import.meta.url),
  EXCERPT_PATTERNS,
  "Written by `bun run analyze:patterns` in packages/envoy-client (test/analyze-patterns.ts): " +
    "recheck's reading of each pattern of dispatch-http.ts's excerpt whose analysis takes more " +
    "than a moment, which excerpt-patterns.test.ts holds to the pattern's source."
);
