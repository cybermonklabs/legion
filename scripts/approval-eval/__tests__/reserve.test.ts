import { afterEach, describe, expect, spyOn, test } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { type CaseRow, countsOf } from "../build-corpus";
import { type Corpus, CorpusError, readCorpus } from "../corpus-io";
import type { DispatchEvent } from "../dispatch-read";
import { FreshError, type FreshReport, refuseReportsOutside } from "../fresh-agent";
import { PoolError, type RunOutcomes, refuseRunsOutside } from "../pool-runs";
import {
  noteIssueCreated,
  pickedKey,
  RESERVE_CUT,
  RESERVE_VARIABLE,
  readReferenceOf,
  reserveOf,
} from "../reserve";

/**
 * The reserve of INFRA-2897's next final test, against the rule as it was registered
 * (dispatch://INFRA/artifact/infra-2897-final-test-reserve-md), and `readCorpus` holding its cases
 * out of what every command reads. Nothing here reads Dispatch or a real corpus.
 */

/** A key the registered rule picks (its digit is 3) and one it does not (its digit is b). */
const PICKED = "INFRA-3601";
const NOT_PICKED = "INFRA-3600";

/** One case, with only the fields the reserve reads given values that matter. */
function caseRow(
  caseId: string,
  issueKey: string | null,
  answeredAt: string,
  issueCreatedAt: string | null | undefined
): CaseRow {
  const row: CaseRow = {
    caseId,
    eventId: 1,
    askId: caseId,
    issueKey,
    project: "INFRA",
    kind: "approval",
    urgency: null,
    multiple: false,
    question: "Approve spec.md (version 2)?",
    options: [],
    selected: ["Approve"],
    words: null,
    hasWords: false,
    askCreatedAt: answeredAt,
    answeredAt,
    eventCreatedAt: answeredAt,
    approval: null,
    verdict: "approve",
    confounds: {
      version: null,
      answersThatDayBefore: 0,
      hourUtc: 0,
      hourPacific: 0,
      changesAlreadyRequested: false,
      priorCasesOnDocument: 0,
      caseKind: "approval",
    },
  };
  if (issueCreatedAt !== undefined) row.issueCreatedAt = issueCreatedAt;
  return row;
}

describe("the registered rule", () => {
  test("picks the registration's worked examples as it says", () => {
    expect(pickedKey(PICKED)).toBe(true);
    expect(pickedKey(NOT_PICKED)).toBe(false);
  });

  test("picks 500 of the 1,000 keys INFRA-3500 to INFRA-4499, as the registration counts", () => {
    let picked = 0;
    for (let number = 3500; number < 4500; number += 1) {
      if (pickedKey(`INFRA-${number}`)) picked += 1;
    }
    expect(picked).toBe(500);
  });

  test("holds a picked issue created at the cut, and keeps one created a millisecond before", () => {
    const after = "2026-11-15T01:00:00Z";
    expect(reserveOf(caseRow("at-cut", PICKED, after, RESERVE_CUT))).toBe("held");
    const before = new Date(Date.parse(RESERVE_CUT) - 1).toISOString();
    expect(reserveOf(caseRow("before-cut", PICKED, after, before))).toBe("kept");
  });

  test("keeps a key the hash does not pick, and a case on no issue", () => {
    const after = "2026-11-15T01:00:00Z";
    expect(reserveOf(caseRow("not-picked", NOT_PICKED, after, after))).toBe("kept");
    expect(reserveOf(caseRow("no-issue", null, after, null))).toBe("kept");
  });

  test("keeps an issue a read moved into the development data", () => {
    const after = "2026-11-15T01:00:00Z";
    expect(pickedKey("INFRA-3689")).toBe(true);
    expect(reserveOf(caseRow("moved", "INFRA-3689", after, "2026-11-14T23:02:33Z"))).toBe("kept");
  });

  test("holds a picked issue whose creation the build did not find", () => {
    expect(reserveOf(caseRow("not-found", PICKED, "2026-11-15T01:00:00Z", null))).toBe("held");
  });

  test("cannot say for a corpus built before creation times, unless the answer predates the cut", () => {
    expect(reserveOf(caseRow("old-build", PICKED, "2026-11-15T01:00:00Z", undefined))).toBe(
      "unknown"
    );
    expect(reserveOf(caseRow("old-answer", PICKED, "2026-11-13T01:00:00Z", undefined))).toBe(
      "kept"
    );
  });
});

describe("readReferenceOf", () => {
  test("holds the reserve when the variable is unset or empty", () => {
    expect(readReferenceOf(undefined)).toBeNull();
    expect(readReferenceOf("  ")).toBeNull();
  });

  test("reads it only under a named reference", () => {
    expect(readReferenceOf("read: dispatch://INFRA-2897/comment/x ")).toBe(
      "dispatch://INFRA-2897/comment/x"
    );
    expect(readReferenceOf("read:")).toBeUndefined();
    expect(readReferenceOf("yes")).toBeUndefined();
  });
});

describe("noteIssueCreated", () => {
  function event(id: number, type: string, key: string | null, payload = {}): DispatchEvent {
    return {
      id,
      issue_key: key,
      project: "INFRA",
      seq: id,
      type,
      actor: null,
      created_at: `2026-11-15T00:00:0${id}Z`,
      payload,
    };
  }

  test("keeps the first creation of a key, by the payload's own time", () => {
    const times = new Map<string, string>();
    const at = (time: string): { created_at: string } => ({ created_at: time });
    noteIssueCreated(times, event(1, "issue.updated", "INFRA-1"));
    noteIssueCreated(times, event(2, "issue.created", "INFRA-1", at("2026-11-14T07:00:00Z")));
    noteIssueCreated(times, event(3, "issue.created", "INFRA-1", at("2026-11-14T08:00:00Z")));
    noteIssueCreated(times, event(4, "issue.created", "INFRA-2"));
    expect([...times]).toEqual([
      ["INFRA-1", "2026-11-14T07:00:00Z"],
      ["INFRA-2", "2026-11-15T00:00:04Z"],
    ]);
  });
});

/** The directories this file made, each removed by its own exact path. */
const made: string[] = [];
const variableBefore = process.env[RESERVE_VARIABLE];

afterEach(async () => {
  for (const directory of made.splice(0)) await rm(directory, { recursive: true, force: true });
  if (variableBefore === undefined) delete process.env[RESERVE_VARIABLE];
  else process.env[RESERVE_VARIABLE] = variableBefore;
});

/** A corpus of the given cases, written where `--corpus` reads one back from. */
async function corpusOnDisk(rows: readonly CaseRow[]): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), "reserve-"));
  made.push(directory);
  await writeFile(
    join(directory, "snapshot.json"),
    `${JSON.stringify({ person: "someone", throughEventId: 1 })}\n`,
    "utf8"
  );
  await writeFile(
    join(directory, "corpus.jsonl"),
    `${rows.map((row) => JSON.stringify(row)).join("\n")}\n`,
    "utf8"
  );
  return directory;
}

/** `readCorpus` with what it printed to stderr. */
async function read(directory: string): Promise<{ corpus: Corpus; printed: string }> {
  const lines: string[] = [];
  const error = spyOn(console, "error").mockImplementation((line?: unknown) => {
    lines.push(String(line));
  });
  try {
    return { corpus: await readCorpus(directory), printed: lines.join("\n") };
  } finally {
    error.mockRestore();
  }
}

describe("readCorpus and the reserve", () => {
  const after = "2026-11-15T01:00:00Z";
  const reserved = caseRow("reserved", PICKED, after, "2026-11-14T09:00:00Z");
  const sameIssueQuestion: CaseRow = {
    ...caseRow("question", PICKED, after, "2026-11-14T09:00:00Z"),
    kind: "question",
  };
  const open = caseRow("open", NOT_PICKED, after, "2026-11-14T09:00:00Z");

  test("holds every case of a reserved issue out of the rows and the ids, and says so", async () => {
    delete process.env[RESERVE_VARIABLE];
    const directory = await corpusOnDisk([reserved, sameIssueQuestion, open]);
    const { corpus, printed } = await read(directory);
    expect(corpus.rows.map((row) => row.caseId)).toEqual(["open"]);
    expect(corpus.byCaseId.has("reserved")).toBe(false);
    expect(corpus.byCaseId.has("question")).toBe(false);
    expect(corpus.reserve).toEqual({ cases: 2, issues: 1, readUnder: null });
    expect(printed).toContain("held out 2 cases on 1 issue");
  });

  test("reads them under a named reference, and records the reference", async () => {
    process.env[RESERVE_VARIABLE] = "read:dispatch://INFRA-2897/comment/claim-run";
    const { corpus } = await read(await corpusOnDisk([reserved, open]));
    expect(corpus.rows.map((row) => row.caseId)).toEqual(["reserved", "open"]);
    expect(corpus.reserve.readUnder).toBe("dispatch://INFRA-2897/comment/claim-run");
  });

  test("refuses a value of the variable that names no reference", async () => {
    process.env[RESERVE_VARIABLE] = "read";
    await expect(readCorpus(await corpusOnDisk([open]))).rejects.toThrow(CorpusError);
  });

  test("refuses a corpus built before creation times that holds a case the rule could hold", async () => {
    delete process.env[RESERVE_VARIABLE];
    const oldBuild = caseRow("old-build", PICKED, after, undefined);
    await expect(readCorpus(await corpusOnDisk([oldBuild, open]))).rejects.toThrow(
      /build it again/
    );
  });

  test("reads a corpus built before the cut as it always did", async () => {
    delete process.env[RESERVE_VARIABLE];
    const old = caseRow("old", PICKED, "2026-11-10T01:00:00Z", undefined);
    const { corpus, printed } = await read(await corpusOnDisk([old]));
    expect(corpus.rows.map((row) => row.caseId)).toEqual(["old"]);
    expect(corpus.reserve.cases).toBe(0);
    expect(printed).toBe("");
  });
});

describe("what a command prints from a file an earlier run wrote", () => {
  const after = "2026-11-15T01:00:00Z";

  test("a build's counts leave a reserved case's verdict out", () => {
    const reserved: CaseRow = {
      ...caseRow("reserved", PICKED, after, "2026-11-14T09:00:00Z"),
      verdict: "request_changes",
    };
    const counts = countsOf([reserved, caseRow("open", NOT_PICKED, after, after)]);
    expect(counts.reserved).toBe(1);
    expect(counts.total).toBe(1);
    expect(counts.approvalByVerdict).toEqual({ approve: 1, request_changes: 0, other: 0 });
  });

  /** The ids a read of a corpus holding one reserved and one open case leaves readable. */
  async function readable(): Promise<Map<string, CaseRow>> {
    delete process.env[RESERVE_VARIABLE];
    const reserved = caseRow("reserved", PICKED, after, "2026-11-14T09:00:00Z");
    const open = caseRow("open", NOT_PICKED, after, "2026-11-14T09:00:00Z");
    return (await read(await corpusOnDisk([reserved, open]))).corpus.byCaseId;
  }

  test("fresh-agent refuses a kept report over a reserved case or one the corpus never had", async () => {
    const corpus = await readable();
    const report = (caseIds: string[]): FreshReport => ({ caseIds }) as FreshReport;
    for (const outside of ["reserved", "of-another-corpus"]) {
      const shapes = { without: report(["open", outside]) };
      expect(() => refuseReportsOutside(shapes, corpus, "/nowhere")).toThrow(FreshError);
    }
    expect(() =>
      refuseReportsOutside({ without: report(["open"]) }, corpus, "/nowhere")
    ).not.toThrow();
  });

  test("pool-runs refuses a run over a reserved case or one the corpus never had", async () => {
    const corpus = await readable();
    const run = (ids: string[]): RunOutcomes => ({
      directory: "/nowhere/run",
      cases: new Map(ids.map((id) => [id, { skill: true, control: false }])),
      finishedAt: null,
      skillSha: null,
    });
    for (const outside of ["reserved", "of-another-corpus"]) {
      expect(() => refuseRunsOutside([run(["open"]), run([outside])], corpus)).toThrow(PoolError);
    }
    expect(() => refuseRunsOutside([run(["open"])], corpus)).not.toThrow();
  });
});
