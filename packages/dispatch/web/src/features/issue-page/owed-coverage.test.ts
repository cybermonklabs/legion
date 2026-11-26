import { expect, test } from "bun:test";

import type { IssueCoverage } from "../../api/types";
import { coverageLineWords, coverageNotes, GAP_LINES } from "./page-words";

// The line the coverage read writes for a document of the issue that owes its block index, in
// place of every missing check and plan task while it does (the server's `owedGap`).
const OWED_LINE = "spec.md version 12 is still being indexed; its coverage shows once that ends.";

// While a document owes its block index the server names no requirement as lacking a check, and
// the page draws none as lacking one either: the owed line is drawn in its own words, which name
// the document and its version, and never as the gap of a requirement no check serves. It is one
// of the coverage read's own lines, which the Spec tab draws over its requirements and the
// outline in each statement's body (`coverageNotes`), and a requirement's own line of a gap is no
// such line. A requirement's line of that gap is still drawn as that gap, so the drawing tells
// the two apart rather than dropping every line it is handed.
test("an owed document line draws its own words and no check gap", () => {
  const drawn = coverageLineWords(OWED_LINE);
  const failure = `the page drew ${drawn ?? "nothing"} for the owed line ${OWED_LINE}`;
  expect(drawn, failure).toBe(OWED_LINE);
  expect(drawn, failure).not.toBe(GAP_LINES.no_check);
  expect(coverageLineWords("r-owed has no check")).toBe(GAP_LINES.no_check);

  const coverage: IssueCoverage = {
    gaps: [OWED_LINE, "r-owed has no plan task", "r-view is a requirement on a view"],
    has_plan: true,
    issue: "CORE-1",
    owed_documents: [{ artifact_id: "artifact-spec", version: 12 }],
    requirements: [],
    spec_artifact_id: "artifact-spec",
  };
  const notes = coverageNotes(coverage);
  expect(notes, `the page drew ${JSON.stringify(notes)} for the owed line ${OWED_LINE}`).toEqual([
    OWED_LINE,
  ]);
});
