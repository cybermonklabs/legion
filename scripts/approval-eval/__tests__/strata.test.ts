import { describe, expect, test } from "bun:test";
import type { CaseKind, CaseRow, CaseVerdict } from "../build-corpus";
import { MINIMUM_DECIDED } from "../stats";
import {
  answerOf,
  approvalCases,
  CUTS,
  crossings,
  QUESTION,
  RATE,
  readStrata,
  type StratumCut,
  strataRecord,
  strataTable,
  stratumLine,
} from "../strata";

/**
 * The strata reading over hand-written rows: nothing here reads a corpus directory, a round, or
 * Dispatch, and nothing makes a call. What it holds the reading to is the rules the reading
 * claims - the request-changes rate of a stratum with its exact bound and its share of the
 * approval volume, the floor of 45 cases under which no bound is printed, the bands each cut
 * places a case in, a crossing kept only where it beats both of the bands it was made of, and
 * the answer line in both directions.
 *
 * The bounds below were computed a second way, by the exact binomial sum the Clopper-Pearson
 * bound inverts, in pure Python:
 *
 * ```
 * python3 -c "
 * from math import comb
 * def cp_upper(f, n, c=0.9):
 *     lo, hi = 0.0, 1.0
 *     for _ in range(200):
 *         mid = (lo + hi) / 2
 *         s = sum(comb(n, k) * mid ** k * (1 - mid) ** (n - k) for k in range(f + 1))
 *         lo, hi = (mid, hi) if s > 1 - c else (lo, mid)
 *     return (lo + hi) / 2
 * for f, n in [(6, 60), (0, 45), (0, 50), (2, 50), (0, 44), (3, 60)]:
 *     print(f, n, '%.6f' % cp_upper(f, n))
 * "
 * ```
 *
 * which prints 0.168927 for 6 of 60, 0.049881 for none of 45, 0.045007 for none of 50, 0.102959
 * for 2 of 50, 0.050986 for none of 44 and 0.107965 for 3 of 60. The floor of 45 and the bar of
 * 5 percent meet there: a stratum of 44 clean cases would still carry a bound over 5 percent, so
 * no stratum the floor hides could have answered the question yes.
 */

/** How far a bound may sit from the reading computed by the binomial sum. */
const TOLERANCE = 1e-5;

/** The two labels every case of this file offers, which is what a verdict is one of. */
const APPROVE = "Approve";
const CHANGES = "Request changes";

/** One case of the corpus, with only the fields the strata read named. */
function caseRow(
  caseId: string,
  options: {
    kind?: CaseKind;
    verdict?: CaseVerdict | null;
    project?: string | null;
    urgency?: string | null;
    version?: number | null;
    priorCasesOnDocument?: number;
    changesAlreadyRequested?: boolean;
    answersThatDayBefore?: number;
    hourPacific?: number;
    documents?: number;
    question?: string;
    answeredAt?: string;
  } = {}
): CaseRow {
  const kind = options.kind ?? "approval";
  const verdict = options.verdict === undefined ? "approve" : options.verdict;
  const documents = options.documents ?? 1;
  return {
    caseId,
    eventId: 1,
    askId: `ask-${caseId}`,
    issueKey: "PROJ-1",
    project: options.project === undefined ? "INFRA" : options.project,
    kind,
    urgency: options.urgency === undefined ? "high" : options.urgency,
    multiple: false,
    question: options.question ?? `Approve the spec of ${caseId}?`,
    options: [
      { id: "o1", label: APPROVE, description: "it is ready" },
      { id: "o2", label: CHANGES, description: "it is not" },
    ],
    selected: [verdict === "request_changes" ? CHANGES : APPROVE],
    words: null,
    hasWords: false,
    askCreatedAt: "2026-11-06T09:00:00Z",
    answeredAt: options.answeredAt ?? "2026-11-06T12:00:00Z",
    eventCreatedAt: options.answeredAt ?? "2026-11-06T12:00:00Z",
    approval: {
      documents: Array.from({ length: documents }, (_, at) => ({
        artifactId: `${caseId}-document-${at}`,
        name: `spec-${at}.md`,
        version: 3,
        requestedVersion: 3,
      })),
      artifactSlug: "spec",
      artifactPrimary: true,
    },
    verdict,
    confounds: {
      version: options.version === undefined ? 3 : options.version,
      answersThatDayBefore: options.answersThatDayBefore ?? 10,
      hourUtc: 18,
      hourPacific: options.hourPacific ?? 10,
      changesAlreadyRequested: options.changesAlreadyRequested ?? false,
      priorCasesOnDocument: options.priorCasesOnDocument ?? 0,
      caseKind: kind,
    },
  };
}

/** `count` cases, the first `changes` of them answered with a request for changes. */
function caseRows(
  prefix: string,
  count: number,
  changes: number,
  options: Parameters<typeof caseRow>[1] = {}
): CaseRow[] {
  return Array.from({ length: count }, (_, at) =>
    caseRow(`${prefix}-${at}`, {
      ...options,
      verdict: at < changes ? "request_changes" : "approve",
    })
  );
}

/** The cut of that name, which every test that reads one names exactly. */
function cutNamed(name: string): StratumCut {
  const cut = CUTS.find((one) => one.name === name);
  if (cut === undefined) throw new RangeError(`no cut is named ${name}`);
  return cut;
}

describe("stratumLine", () => {
  test("counts the rate, the exact bound and the share of the approval volume", () => {
    const line = stratumLine("six of sixty", caseRows("a", 60, 6), 120);
    expect(line.cases).toBe(60);
    expect(line.requestChanges).toBe(6);
    expect(line.rate).toBeCloseTo(0.1, 10);
    expect(line.bound).not.toBeNull();
    expect(line.bound ?? 0).toBeCloseTo(0.168927, 5);
    expect(line.share).toBeCloseTo(0.5, 10);
  });

  test("a stratum of 45 clean cases is bounded just under the bar, and one of 44 just over", () => {
    const clean = stratumLine("clean", caseRows("b", MINIMUM_DECIDED, 0), MINIMUM_DECIDED);
    expect(clean.bound ?? 0).toBeCloseTo(0.049881, 5);
    expect(clean.bound ?? 1).toBeLessThan(RATE);
    const short = stratumLine("short", caseRows("c", MINIMUM_DECIDED - 1, 0), MINIMUM_DECIDED);
    expect(short.cases).toBe(44);
    expect(short.bound).toBeNull();
  });

  test("a stratum with no case carries no bound and no rate", () => {
    const line = stratumLine("none", [], 10);
    expect(line).toEqual({
      name: "none",
      cases: 0,
      requestChanges: 0,
      rate: 0,
      bound: null,
      share: 0,
    });
  });
});

describe("CUTS", () => {
  test("every cut places a case by facts the request carries before it is answered", () => {
    const placed = (name: string, row: CaseRow): string | null => cutNamed(name).bandOf(row);
    expect(placed("project", caseRow("p", { project: "OSSV" }))).toBe("OSSV");
    expect(placed("project", caseRow("p", { project: null }))).toBe("no project");
    expect(placed("version", caseRow("v", { version: 1 }))).toBe("a first version");
    expect(placed("version", caseRow("v", { version: 4 }))).toBe("a later version");
    expect(placed("version", caseRow("v", { version: null }))).toBe("no version");
    const already = "changes already requested";
    expect(placed(already, caseRow("c", { changesAlreadyRequested: true }))).toBe(already);
    expect(placed(already, caseRow("c", {}))).toBe("no changes requested yet");
    const prior = "earlier cases on this document";
    expect(placed(prior, caseRow("d", { priorCasesOnDocument: 0 }))).toBe("none");
    expect(placed(prior, caseRow("d", { priorCasesOnDocument: 1 }))).toBe("one");
    expect(placed(prior, caseRow("d", { priorCasesOnDocument: 7 }))).toBe("two and more");
    expect(placed("documents covered", caseRow("e", { documents: 1 }))).toBe("one document");
    expect(placed("documents covered", caseRow("e", { documents: 2 }))).toBe("two documents");
    expect(placed("documents covered", caseRow("e", { documents: 5 }))).toBe("three and more");
    const bytes = "request bytes";
    expect(placed(bytes, caseRow("f", { question: "x".repeat(399) }))).toBe("under 400 bytes");
    expect(placed(bytes, caseRow("f", { question: "x".repeat(400) }))).toBe("400 to 699 bytes");
    expect(placed(bytes, caseRow("f", { question: "x".repeat(700) }))).toBe("700 to 1,099 bytes");
    expect(placed(bytes, caseRow("f", { question: "x".repeat(1100) }))).toBe(
      "1,100 bytes and more"
    );
    const hour = "hour of the day where the person is";
    expect(placed(hour, caseRow("g", { hourPacific: 3 }))).toBe("00 to 06, the night");
    expect(placed(hour, caseRow("g", { hourPacific: 9 }))).toBe("07 to 10, the morning");
    expect(placed(hour, caseRow("g", { hourPacific: 12 }))).toBe("11 to 13, the middle of the day");
    expect(placed(hour, caseRow("g", { hourPacific: 16 }))).toBe("14 to 17, the afternoon");
    expect(placed(hour, caseRow("g", { hourPacific: 22 }))).toBe("18 to 23, the evening");
    const day = "decisions already answered that day";
    expect(placed(day, caseRow("h", { answersThatDayBefore: 0 }))).toBe("none yet that day");
    expect(placed(day, caseRow("h", { answersThatDayBefore: 9 }))).toBe("1 to 9 that day");
    expect(placed(day, caseRow("h", { answersThatDayBefore: 24 }))).toBe("10 to 24 that day");
    expect(placed(day, caseRow("h", { answersThatDayBefore: 49 }))).toBe("25 to 49 that day");
    expect(placed(day, caseRow("h", { answersThatDayBefore: 99 }))).toBe("50 to 99 that day");
    expect(placed(day, caseRow("h", { answersThatDayBefore: 100 }))).toBe("100 and more that day");
    expect(placed("urgency", caseRow("i", { urgency: "med" }))).toBe("med");
    expect(placed("urgency", caseRow("i", { urgency: null }))).toBe("no urgency");
  });

  test("the bytes a request is banded by are the bytes of its words, not its characters", () => {
    const row = caseRow("j", { question: "é".repeat(200) });
    expect(cutNamed("request bytes").bandOf(row)).toBe("400 to 699 bytes");
  });
});

describe("strataTable", () => {
  test("one line per band, in the order the cut declares, each against the whole volume", () => {
    const rows = [
      ...caseRows("one", 50, 0, { priorCasesOnDocument: 1 }),
      ...caseRows("none", 50, 25, { priorCasesOnDocument: 0 }),
    ];
    const table = strataTable(cutNamed("earlier cases on this document"), rows, rows.length);
    expect(table.lines.map((line) => line.name)).toEqual(["none", "one"]);
    expect(table.lines.map((line) => line.cases)).toEqual([50, 50]);
    expect(table.lines.map((line) => line.share)).toEqual([0.5, 0.5]);
    expect(table.lines[1]?.bound ?? 1).toBeCloseTo(0.045007, 5);
    expect(table.answer).toStartWith("Yes.");
    expect(table.answer).toContain("0.500 of the approval volume");
  });

  test("a band holding no case of the set is not a line of its table", () => {
    const rows = caseRows("later", 10, 0, { version: 5 });
    const table = strataTable(cutNamed("version"), rows, rows.length);
    expect(table.lines.map((line) => line.name)).toEqual(["a later version"]);
  });
});

describe("answerOf", () => {
  test("says yes, with the count, the bound and the share, where a stratum holds the rate", () => {
    const lines = [
      stratumLine("clean", caseRows("y", 50, 0), 100),
      stratumLine("dirty", caseRows("z", 50, 20), 100),
    ];
    const answer = answerOf(lines, { one: "stratum", many: "strata" });
    expect(answer).toStartWith("Yes. 1 of 2 strata hold a request-changes rate under 5 percent");
    expect(answer).toContain("clean (50 cases, 0 of them request changes, rate 0.000");
    expect(answer).toContain("bound at most 0.045");
    expect(answer).toContain("0.500 of the approval volume");
  });

  test("says no, with the lowest bound and the count behind it, where none does", () => {
    const lines = [
      stratumLine("lowest", caseRows("y", 60, 3), 120),
      stratumLine("higher", caseRows("z", 60, 12), 120),
    ];
    const answer = answerOf(lines, { one: "stratum", many: "strata" });
    expect(answer).toStartWith("No. No stratum holds a request-changes rate under 5 percent");
    expect(answer).toContain("The lowest bound any of them reaches is 0.108, over lowest, at 60 ");
    expect(answer).toContain("60 cases of which 3 request changes");
    expect(answer).toContain("0.500 of the approval volume");
  });

  test("names the strata too small to bound, and says so when none of them carries one", () => {
    const lines = [stratumLine("small", caseRows("y", 10, 0), 10)];
    const answer = answerOf(lines, { one: "stratum", many: "strata" });
    expect(answer).toBe(
      `No. No stratum holds ${MINIMUM_DECIDED} cases, so none of them carries a bound at all. ` +
        "1 of 1 holds fewer than 45 cases and so carries no bound."
    );
  });
});

describe("crossings", () => {
  test("keeps a pair that beats both bands it was made of, and names both cuts", () => {
    // The evening is cleaner than the day, and a document with one earlier case cleaner than one
    // with none: neither band alone is under the bar, and the two crossed are.
    const rows = [
      ...caseRows("evening-one", 50, 0, { hourPacific: 20, priorCasesOnDocument: 1 }),
      ...caseRows("evening-none", 50, 15, { hourPacific: 20, priorCasesOnDocument: 0 }),
      ...caseRows("morning-one", 50, 15, { hourPacific: 9, priorCasesOnDocument: 1 }),
      ...caseRows("morning-none", 50, 25, { hourPacific: 9, priorCasesOnDocument: 0 }),
    ];
    const cuts = [
      cutNamed("hour of the day where the person is"),
      cutNamed("earlier cases on this document"),
    ];
    const table = crossings(cuts, rows, rows.length);
    const best = table.lines[0];
    expect(best?.name).toBe(
      "hour of the day where the person is = 18 to 23, the evening + " +
        "earlier cases on this document = one"
    );
    expect(best?.cases).toBe(50);
    expect(best?.requestChanges).toBe(0);
    expect(best?.bound ?? 1).toBeCloseTo(0.045007, 5);
    expect(best?.share).toBeCloseTo(0.25, 10);
    expect(table.answer).toStartWith("Yes.");
  });

  test("leaves out a pair under the floor, and a pair that beats neither of its bands", () => {
    const rows = [
      ...caseRows("evening-one", 44, 0, { hourPacific: 20, priorCasesOnDocument: 1 }),
      ...caseRows("evening-none", 50, 10, { hourPacific: 20, priorCasesOnDocument: 0 }),
      ...caseRows("morning-none", 50, 10, { hourPacific: 9, priorCasesOnDocument: 0 }),
    ];
    const cuts = [
      cutNamed("hour of the day where the person is"),
      cutNamed("earlier cases on this document"),
    ];
    const table = crossings(cuts, rows, rows.length);
    expect(table.lines.map((line) => line.name)).toEqual([]);
    expect(table.answer).toStartWith("No. No crossing holds 45 cases");
  });
});

describe("readStrata", () => {
  test("answers over every stratum of the reading, the crossings included", () => {
    const rows = [
      ...caseRows("clean", 50, 0, { project: "OSSV" }),
      ...caseRows("dirty", 50, 20, { project: "INFRA" }),
    ];
    const reading = readStrata("A reading", "what it is over", rows, [cutNamed("project")]);
    expect(reading.cases).toBe(100);
    expect(reading.requestChanges).toBe(20);
    expect(reading.rate).toBeCloseTo(0.2, 10);
    expect(reading.tables.map((table) => table.name)).toEqual(["project"]);
    expect(reading.answer).toStartWith("Yes.");
    expect(reading.answer).toContain("OSSV (50 cases");
    expect(reading.crossings.lines).toEqual([]);
  });
});

describe("approvalCases", () => {
  test("takes the approval cases the person answered one way or the other, and no other", () => {
    const rows = [
      caseRow("approved"),
      caseRow("changed", { verdict: "request_changes" }),
      caseRow("other", { verdict: "other" }),
      caseRow("unanswered", { verdict: null }),
      caseRow("question", { kind: "question", verdict: null }),
    ];
    expect(approvalCases(rows).map((row) => row.caseId)).toEqual(["approved", "changed"]);
  });
});

describe("strataRecord", () => {
  test("reads the whole corpus, and the part after the cut as a reading of its own", () => {
    const rows = [
      ...caseRows("before", 50, 20, { answeredAt: "2026-11-01T12:00:00Z" }),
      ...caseRows("after", 50, 0, { answeredAt: "2026-11-06T12:00:00Z" }),
      caseRow("question", { kind: "question", verdict: null }),
    ];
    const record = strataRecord(rows, {
      corpus: "/somewhere/run1",
      person: "someone@example.test",
      cut: "2026-11-05T00:00:00.000Z",
      cutFrom: "--cut",
      ranAt: "2026-11-12T00:00:00.000Z",
    });
    expect(record.approvalCases).toBe(100);
    expect(record.withoutVerdict).toBe(0);
    expect(record.readings.map((reading) => reading.name)).toEqual([
      "The whole corpus",
      "After the cut",
    ]);
    expect(record.readings[0]?.cases).toBe(100);
    expect(record.readings[0]?.requestChanges).toBe(20);
    expect(record.readings[1]?.cases).toBe(50);
    expect(record.readings[1]?.requestChanges).toBe(0);
    expect(record.readings[1]?.bound ?? 1).toBeCloseTo(0.045007, 5);
  });

  test("makes one reading alone where no cut is known", () => {
    const record = strataRecord(caseRows("a", 10, 1), {
      corpus: "/somewhere/run1",
      person: "someone@example.test",
      cut: null,
      cutFrom: "no --cut and no draw in this corpus, so only the whole corpus is read",
      ranAt: "2026-11-12T00:00:00.000Z",
    });
    expect(record.readings.map((reading) => reading.name)).toEqual(["The whole corpus"]);
  });

  test("counts the approval cases the person left no verdict on, and reads over none of them", () => {
    const rows = [...caseRows("a", 10, 1), caseRow("other", { verdict: "other" })];
    const record = strataRecord(rows, {
      corpus: "/somewhere/run1",
      person: "someone@example.test",
      cut: null,
      cutFrom: "--cut",
      ranAt: "2026-11-12T00:00:00.000Z",
    });
    expect(record.approvalCases).toBe(10);
    expect(record.withoutVerdict).toBe(1);
  });
});

describe("the question", () => {
  test("is asked in the words the reading reports it in", () => {
    expect(QUESTION).toBe(
      "Does any identifiable stratum hold a request-changes rate under 5 percent, and what " +
        "share of the volume is in it?"
    );
    expect(RATE).toBe(0.05);
  });

  test("a bound within tolerance of the binomial sum is what every answer is read off", () => {
    const line = stratumLine("two of fifty", caseRows("t", 50, 2), 50);
    expect(Math.abs((line.bound ?? 0) - 0.102959)).toBeLessThan(TOLERANCE);
  });
});
