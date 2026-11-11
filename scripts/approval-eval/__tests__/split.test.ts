import { describe, expect, test } from "bun:test";
import type { CaseRow } from "../build-corpus";
import { evidenceFor } from "../split";

/**
 * The dating rule over a hand-written corpus, which is the whole of this test: nothing here
 * reads a corpus from disk and nothing here reads Dispatch.
 */

const CUT = "2026-11-05T00:00:00Z";

/** One case, as `corpus.jsonl` holds one, with only the fields the dating rule reads named. */
function caseRow(caseId: string, answeredAt: string, kind: CaseRow["kind"] = "approval"): CaseRow {
  return {
    caseId,
    eventId: 1,
    askId: caseId,
    issueKey: "PROJ-1",
    project: "PROJ",
    kind,
    urgency: null,
    multiple: false,
    question: "Approve it?",
    options: [],
    selected: ["Approve"],
    words: null,
    hasWords: false,
    askCreatedAt: answeredAt,
    answeredAt,
    eventCreatedAt: answeredAt,
    approval: null,
    verdict: kind === "approval" ? "approve" : null,
    confounds: {
      version: null,
      answersThatDayBefore: 0,
      hourUtc: 0,
      hourPacific: 0,
      changesAlreadyRequested: false,
      priorCasesOnDocument: 0,
      caseKind: kind,
    },
  };
}

/** The judged case sits on the training side, so each other case is left out for one reason. */
const judged = caseRow("judged", "2026-11-04T12:00:00Z");
const earlier = caseRow("earlier", "2026-11-01T09:00:00Z", "question");
const sameMoment = caseRow("same-moment", "2026-11-04T12:00:00Z");
const later = caseRow("later", "2026-11-04T18:00:00Z");
const pastTheCut = caseRow("past-the-cut", "2026-11-06T08:00:00Z");
const corpus = [earlier, sameMoment, judged, later, pastTheCut];

describe("evidenceFor", () => {
  test("allows only the cases answered earlier, on the training side", () => {
    const evidence = evidenceFor(corpus, judged, { cut: CUT });
    expect(evidence.map((row) => row.caseId)).toEqual(["earlier"]);
  });

  test("leaves out a case answered at the judged case's own moment", () => {
    const evidence = evidenceFor(corpus, judged, { cut: CUT });
    expect(evidence.map((row) => row.caseId)).not.toContain("same-moment");
  });

  test("leaves out a case answered later and a case past the cut", () => {
    const allowed = evidenceFor(corpus, judged, { cut: CUT }).map((row) => row.caseId);
    expect(allowed).not.toContain("later");
    expect(allowed).not.toContain("past-the-cut");
  });

  test("never allows the judged case as its own evidence", () => {
    expect(evidenceFor(corpus, judged, { cut: CUT }).map((row) => row.caseId)).not.toContain(
      "judged"
    );
    expect(
      evidenceFor(corpus, judged, { cut: CUT, datingRule: false }).map((row) => row.caseId)
    ).not.toContain("judged");
  });

  test("the control drops the time test and keeps the cut", () => {
    const control = evidenceFor(corpus, judged, { cut: CUT, datingRule: false });
    expect(control.map((row) => row.caseId)).toEqual(["earlier", "same-moment", "later"]);
  });

  test("reads the cut as a Date as well as a string", () => {
    const evidence = evidenceFor(corpus, judged, { cut: new Date(CUT) });
    expect(evidence.map((row) => row.caseId)).toEqual(["earlier"]);
  });

  test("judging a case past the cut sees the whole training side", () => {
    const evidence = evidenceFor(corpus, pastTheCut, { cut: CUT });
    expect(evidence.map((row) => row.caseId)).toEqual([
      "earlier",
      "same-moment",
      "judged",
      "later",
    ]);
  });

  test("refuses a case whose answer time is no time", () => {
    const broken = caseRow("broken", "not a time");
    expect(() => evidenceFor([earlier], broken, { cut: CUT })).toThrow(/no time/);
  });

  test("refuses a cut that is no time", () => {
    expect(() => evidenceFor(corpus, judged, { cut: "whenever" })).toThrow(/ISO 8601/);
  });
});
