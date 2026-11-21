import { describe, expect, test } from "bun:test";
import {
  DISPATCH_BLOCK_REF_PATTERNS,
  DISPATCH_HREF_PATTERNS,
  DISPATCH_TEXT_RULE_PATTERNS,
} from "@legion/contracts";
import { analyzePattern } from "@legion/envoy-client/test/pattern-analysis";

import { REFERENCE_SPAN_PATTERNS } from "./reference-spans";
import { ROUTE_PATTERNS } from "./routes";
import { UNFURL_PATTERNS } from "./Unfurl";

/**
 * Every regular expression the readers of a body's references run on what an author wrote: the
 * span pattern every body is read with, each pattern `referenceRouteFromHref` and `readAddress`
 * run (`routes.ts`, and `@legion/contracts`' control-character, session-id, id-prefix and pin
 * rules), and the GitHub path and word patterns of `Unfurl`. `trimReference` and
 * `trimmedMention` run none: each is a loop the reads counter counts (`reference-reads.ts`).
 */
const READER_PATTERNS: Readonly<Record<string, Readonly<Record<string, RegExp>>>> = {
  "@legion/contracts dispatch-block-ref.ts": DISPATCH_BLOCK_REF_PATTERNS,
  "@legion/contracts dispatch-href.ts": DISPATCH_HREF_PATTERNS,
  "@legion/contracts dispatch-text-rules.ts": DISPATCH_TEXT_RULE_PATTERNS,
  "Unfurl.tsx": UNFURL_PATTERNS,
  "reference-spans.ts": REFERENCE_SPAN_PATTERNS,
  "routes.ts": ROUTE_PATTERNS,
};

// A body holds up to 1 MiB, and a pattern that backtracks costs more than linear work in what it
// is run on within one call, which no count of calls or characters sees: JavaScriptCore counts no
// step a regular expression takes. recheck's automaton checker decides it from the pattern's
// automaton instead, for every text and not only one a row builds, with no clock of its own
// (`analyzePattern`, `timeout: null`), so its verdict is the same on every machine, however
// loaded. The end-anchored `/[_-]+$/` `trimmedMention` ran before it became a loop reads
// `2nd degree polynomial` here.
describe("each pattern the reference readers run, as recheck's automaton checker reads it", () => {
  for (const [module, patterns] of Object.entries(READER_PATTERNS)) {
    for (const [name, pattern] of Object.entries(patterns)) {
      test(`${module} ${name} is linear in the text`, async () => {
        const analysis = await analyzePattern(pattern);
        expect(
          analysis.status,
          `${module} ${name} /${analysis.source}/${analysis.flags} is ${analysis.status}: ${analysis.verdict}`
        ).toBe("safe");
      });
    }
  }
});
