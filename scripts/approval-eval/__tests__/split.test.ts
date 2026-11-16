import { afterEach, describe, expect, spyOn, test } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { CaseRow } from "../build-corpus";
import { evidenceFor, main } from "../split";

/**
 * The dating rule over a hand-written corpus, and the command over a corpus written into a
 * scratch directory: nothing here reads the real corpus and nothing here reads Dispatch. What
 * the command's own test holds is the negative control itself - `--no-dating-rule` has to move
 * a number of the readout, since a flag that reaches only a printed line controls nothing.
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

/** The directories this file made, each removed by its own exact path. */
const made: string[] = [];

afterEach(async () => {
  for (const directory of made.splice(0)) await rm(directory, { recursive: true, force: true });
});

/** One scratch directory under the scratch volume, never under the workspace. */
async function scratch(): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), "split-"));
  made.push(directory);
  return directory;
}

/** The five cases of this file, written where `--corpus` reads a corpus back from. */
async function corpusOnDisk(): Promise<string> {
  const directory = await scratch();
  await writeFile(
    join(directory, "snapshot.json"),
    `${JSON.stringify({ person: "someone", throughEventId: 1 })}\n`,
    "utf8"
  );
  await writeFile(
    join(directory, "corpus.jsonl"),
    `${corpus.map((row) => JSON.stringify(row)).join("\n")}\n`,
    "utf8"
  );
  return directory;
}

/** What one run of the command printed, with the status it returned. */
async function run(argv: readonly string[]): Promise<{ printed: string; exitCode: number }> {
  const lines: string[] = [];
  const log = spyOn(console, "log").mockImplementation((line?: unknown) => {
    lines.push(String(line));
  });
  let exitCode: number;
  try {
    exitCode = await main(argv);
  } finally {
    log.mockRestore();
  }
  return { printed: lines.join("\n"), exitCode };
}

/** The count one line of the readout carries, which is what the two runs are compared on. */
function allowed(printed: string, label: string): number {
  const read = new RegExp(`${label}\\s+(\\d+)`).exec(printed);
  if (read === null) throw new Error(`the readout carries no ${label} line:\n${printed}`);
  return Number(read[1]);
}

describe("the command's own negative control", () => {
  test("--no-dating-rule moves the evidence count, and not only a printed line", async () => {
    const directory = await corpusOnDisk();
    const argv = [
      "--corpus",
      directory,
      "--cut",
      CUT,
      "--out",
      await scratch(),
      "--case",
      "judged",
    ];
    const ruled = await run(argv);
    const control = await run([...argv, "--no-dating-rule"]);
    expect(ruled.exitCode).toBe(0);
    expect(control.exitCode).toBe(0);
    // One case is answered earlier than the judged one; two more are on the training side but
    // answered at that same moment or later, which only the control lets through.
    expect(allowed(ruled.printed, "this run allows")).toBe(1);
    expect(allowed(control.printed, "this run allows")).toBe(3);
    expect(ruled.printed).toContain("this run allows          1 (the dating rule)");
    expect(control.printed).toContain("this run allows          3 (no dating rule)");
  });

  test("both runs read the same case and state the same two sides of the comparison", async () => {
    const directory = await corpusOnDisk();
    const argv = [
      "--corpus",
      directory,
      "--cut",
      CUT,
      "--out",
      await scratch(),
      "--case",
      "judged",
    ];
    const ruled = await run(argv);
    const control = await run([...argv, "--no-dating-rule"]);
    for (const printed of [ruled.printed, control.printed]) {
      expect(printed).toContain("evidence for      judged");
      expect(allowed(printed, "the dating rule allows")).toBe(1);
      expect(allowed(printed, "the control allows")).toBe(3);
    }
    expect(ruled.printed).toContain("rule in force     the dating rule");
    expect(control.printed).toContain("rule in force     no dating rule");
  });
});
