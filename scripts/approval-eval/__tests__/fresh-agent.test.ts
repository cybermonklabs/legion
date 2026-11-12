import { afterEach, describe, expect, test } from "bun:test";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { CaseRow, CaseVerdict } from "../build-corpus";
import {
  type AgentAnswer,
  type AgentRequest,
  type AnswerRow,
  type ArmName,
  answerOf,
  answersPath,
  type FreshCase,
  type FreshDeps,
  pairedOf,
  promptOf,
  readingOf,
  runFresh,
  verdictLineOf,
} from "../fresh-agent";
import type { ReplayedDocument } from "../replay";
import { mcnemarExact } from "../stats";

/**
 * The two arms over hand-written cases and a fake agent: nothing here starts a process, reads a
 * corpus or an evidence cache, or reaches a model. What it holds the run to is the five rules
 * the check names - both arms see the same cases, the two shapes differ by one sentence and by
 * no other word, the paired difference and its interval are read over a table computed a second
 * way, the verdict line says which way the check went, and an answer in no readable shape is a
 * fault and never a decided case.
 */

/** The directories this file made, each removed by its own exact path. */
const made: string[] = [];

afterEach(async () => {
  for (const directory of made.splice(0)) await rm(directory, { recursive: true, force: true });
});

/** One scratch directory under the scratch volume, never under the workspace. */
async function scratch(): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), "fresh-agent-"));
  made.push(directory);
  return directory;
}

/** The two labels every case of this file offers. */
const APPROVE = "Approve";
const CHANGES = "Request changes";

/** One case, with only the fields the run and the reading name. */
function caseRow(caseId: string, verdict: CaseVerdict = "approve"): CaseRow {
  return {
    caseId,
    eventId: 1,
    askId: `ask-${caseId}`,
    issueKey: "PROJ-1",
    project: "PROJ",
    kind: "approval",
    urgency: "med",
    multiple: false,
    question: `Approve the spec of ${caseId}?`,
    options: [
      { id: "o1", label: APPROVE, description: "it is ready" },
      { id: "o2", label: CHANGES, description: "it is not" },
    ],
    selected: [verdict === "approve" ? APPROVE : CHANGES],
    words: null,
    hasWords: false,
    askCreatedAt: "2026-11-06T09:00:00Z",
    answeredAt: "2026-11-06T12:00:00Z",
    eventCreatedAt: "2026-11-06T12:00:00Z",
    approval: {
      documents: [{ artifactId: `${caseId}-a`, name: "spec.md", version: 3, requestedVersion: 3 }],
      artifactSlug: "spec",
      artifactPrimary: true,
    },
    verdict,
    confounds: {
      version: 3,
      answersThatDayBefore: 0,
      hourUtc: 12,
      hourPacific: 4,
      changesAlreadyRequested: false,
      priorCasesOnDocument: 0,
      caseKind: "approval",
    },
  };
}

/** One case as both arms are given it, with the prompt a fake agent never reads. */
function freshCase(caseId: string, verdict: CaseVerdict = "approve"): FreshCase {
  return {
    row: caseRow(caseId, verdict),
    prompt: `Judge ${caseId}.`,
    labels: [APPROVE, CHANGES],
    directory: `/nowhere/${caseId}`,
  };
}

/** One document of a case, as the round's evidence cache holds it. */
function document(name: string, markdown: string, requestedVersion: number): ReplayedDocument {
  return {
    name,
    artifactId: `artifact-${name}`,
    version: requestedVersion,
    requestedVersion,
    markdown,
    bytes: markdown.length,
    sha256: `sha-of-${name}`,
    versionCreatedAt: "2026-11-05T09:00:00Z",
    named: true,
    summary: null,
    refusal: null,
  };
}

describe("the two shapes of the prompt", () => {
  /** The sentences the `with` shape adds, which are the whole of the difference. */
  const FILE_SENTENCE = " One file sits in the directory you are started in";
  const ENDS = "Whether you read it is yours to decide.";

  test("differ by the sentence about the file and by no other word", async () => {
    const row = caseRow("case-0");
    const documents = [document("spec.md", "# The spec\n\nIt says what it does.", 3)];
    const withFile = await promptOf(row, documents, "The issue", "with");
    const without = await promptOf(row, documents, "The issue", "without");
    expect(withFile).toContain("past-decisions.md");
    expect(without).not.toContain("past-decisions.md");
    expect(without).toContain("Nothing of the record is reachable from this machine");
    const opens = withFile.indexOf(FILE_SENTENCE);
    const closes = withFile.indexOf(ENDS) + ENDS.length;
    expect(opens).toBeGreaterThan(0);
    expect(withFile.slice(0, opens) + withFile.slice(closes)).toBe(without);
  });
});

/** What one fake run saw, so a test can read which arm was asked what. */
interface Fake extends FreshDeps {
  asked: AgentRequest[];
  lines: string[];
}

/** A run's world, with the answer the caller chooses for each arm and case. */
function fake(textOf: (request: AgentRequest) => string): Fake {
  const deps: Fake = {
    asked: [],
    lines: [],
    ask: (request: AgentRequest): Promise<AgentAnswer> => {
      deps.asked.push(request);
      return Promise.resolve({
        text: textOf(request),
        model: "fake-opus",
        latencyMs: 7,
        failure: null,
      });
    },
    now: () => Date.parse("2026-11-12T09:00:00Z"),
    log: (line: string) => {
      deps.lines.push(line);
    },
  };
  return deps;
}

/**
 * A run of `count` cases where the skill arm alone agreed on `armOnly` of them, the control
 * alone on `otherOnly`, and both agreed on the rest. The person approved every case, so an
 * agreeing arm answers `Approve` and a differing one answers `Request changes`.
 */
async function runTable(armOnly: number, otherOnly: number, count: number) {
  const cases = Array.from({ length: count }, (_, at) => freshCase(`case-${at}`));
  const agreesOf = (arm: ArmName, at: number): boolean => {
    if (at < armOnly) return arm === "with-skill";
    if (at < armOnly + otherOnly) return arm === "no-skill";
    return true;
  };
  const deps = fake((request) => {
    const at = Number(request.caseId.slice("case-".length));
    return agreesOf(request.arm, at) ? APPROVE : CHANGES;
  });
  const out = await scratch();
  const report = await runFresh(
    {
      neighbours: "with",
      cases,
      out,
      homes: { "with-skill": "/home-with", "no-skill": "/home-no" },
      concurrency: 4,
    },
    deps
  );
  return { report, deps, cases };
}

describe("both arms see the same cases", () => {
  test("each case is judged once by each arm, over one case list", async () => {
    const { report, deps, cases } = await runTable(2, 1, 5);
    expect(report.caseIds).toEqual(cases.map((one) => one.row.caseId));
    expect(deps.asked).toHaveLength(10);
    for (const arm of ["with-skill", "no-skill"] as const) {
      const seen = deps.asked.filter((one) => one.arm === arm).map((one) => one.caseId);
      expect([...seen].sort()).toEqual([...report.caseIds].sort());
      expect(new Set(seen).size).toBe(seen.length);
    }
    expect(report.arms.map((one) => one.arm)).toEqual(["with-skill", "no-skill"]);
    expect(report.arms.every((one) => one.cases === 5)).toBe(true);
    expect(report.paired.counted).toBe(5);
  });

  test("both arms are sent the same one message, and differ in the home they run under", async () => {
    const { deps } = await runTable(1, 1, 3);
    for (const caseId of ["case-0", "case-1", "case-2"]) {
      const sent = deps.asked.filter((one) => one.caseId === caseId);
      expect(sent).toHaveLength(2);
      expect(sent[0]?.prompt).toBe(sent[1]?.prompt ?? "");
      expect(new Set(sent.map((one) => one.home)).size).toBe(2);
    }
  });
});

describe("the paired difference and the verdict", () => {
  // The interval of each table was computed a second way by `stats.test.ts`, which holds
  // `mcnemarExact` to scipy: these are that test's rows, read through the whole run.
  test("20 cases the skill arm alone won against 5 of 100 differs by +0.150", async () => {
    const { report } = await runTable(20, 5, 100);
    expect(report.paired.armOnly).toBe(20);
    expect(report.paired.otherOnly).toBe(5);
    expect(report.paired.bothAgreed).toBe(75);
    expect(report.paired.difference).toBeCloseTo(0.15, 10);
    expect(100 * report.paired.interval.lower).toBeCloseTo(6.2297, 3);
    expect(100 * report.paired.interval.upper).toBeCloseTo(20.8855, 3);
    expect(report.arms[0]?.agreement).toBeCloseTo(0.95, 10);
    expect(report.arms[1]?.agreement).toBeCloseTo(0.8, 10);
  });

  test("the whole interval above zero is the check passing, in those words", async () => {
    const { report } = await runTable(20, 5, 100);
    expect(report.verdict.passes).toBe(true);
    expect(report.verdict.line).toContain(
      "the check passes: the skill arm beats the control by more than the interval of the difference"
    );
  });

  test("an interval holding zero is the check failing, in those words", async () => {
    const { report } = await runTable(3, 9, 60);
    expect(report.paired.difference).toBeCloseTo(-0.1, 10);
    expect(100 * report.paired.interval.lower).toBeCloseTo(-17.1252, 3);
    expect(100 * report.paired.interval.upper).toBeCloseTo(1.0931, 3);
    expect(report.verdict.passes).toBe(false);
    expect(report.verdict.line).toContain(
      "the check fails: the skill arm does not beat the control by more than the interval of the difference"
    );
  });

  test("a gain the interval does not clear fails, however large the difference reads", () => {
    const won = mcnemarExact([
      { armAgreed: true, otherAgreed: false },
      { armAgreed: true, otherAgreed: true },
    ]);
    expect(won.difference).toBeCloseTo(0.5, 10);
    expect(verdictLineOf(won).passes).toBe(false);
  });
});

describe("an answer in no readable shape", () => {
  test("is a fault, is not a decided case, and is never an agreement", async () => {
    const cases = [freshCase("case-0"), freshCase("case-1")];
    const deps = fake((request) =>
      request.arm === "with-skill" && request.caseId === "case-1"
        ? "I would need to see the issue's label before I could decide this one."
        : APPROVE
    );
    const out = await scratch();
    const report = await runFresh(
      {
        neighbours: "with",
        cases,
        out,
        homes: { "with-skill": "/home-with", "no-skill": "/home-no" },
        concurrency: 1,
      },
      deps
    );
    const skill = report.arms[0];
    expect(skill?.faults).toBe(1);
    expect(skill?.decided).toBe(1);
    expect(skill?.cases).toBe(2);
    expect(skill?.agreed).toBe(1);
    expect(skill?.agreement).toBeCloseTo(0.5, 10);
    expect(skill?.falseApprovals).toBe(0);
    expect(skill?.falseRequests).toBe(0);
    expect(report.arms[1]?.faults).toBe(0);
    expect(report.paired.otherOnly).toBe(1);
  });

  test("a verdict the arm read is counted as the error kind it is", () => {
    const cases = [freshCase("case-0", "request_changes")];
    const rows: AnswerRow[] = [
      {
        arm: "with-skill",
        caseId: "case-0",
        model: "fake-opus",
        chosen: [APPROVE],
        agreed: false,
        fault: null,
        faultDetail: null,
        latencyMs: 7,
        at: "2026-11-12T09:00:00.000Z",
      },
    ];
    const reading = readingOf("with-skill", rows, cases);
    expect(reading.falseApprovals).toBe(1);
    expect(reading.falseRequests).toBe(0);
    expect(reading.decided).toBe(1);
    expect(reading.faults).toBe(0);
  });

  test("a case one arm has no answer for is a refusal and never a pairing", () => {
    const cases = [freshCase("case-0")];
    expect(() => pairedOf([], cases)).toThrow("has no answer for the case case-0");
  });
});

describe("the agent's own stream", () => {
  test("the last text it printed is the answer, with the model it says it ran on", () => {
    const stream = [
      JSON.stringify({ type: "session", id: "one" }),
      JSON.stringify({ type: "message_start", message: { role: "user", content: [] } }),
      JSON.stringify({
        type: "message_end",
        message: { role: "assistant", content: [{ type: "text", text: "Reading the case." }] },
      }),
      "not json at all",
      JSON.stringify({
        type: "turn_end",
        message: {
          role: "assistant",
          model: "claude-opus-5-5",
          content: [{ type: "text", text: `${APPROVE}\n` }],
        },
      }),
    ].join("\n");
    expect(answerOf(stream)).toEqual({ text: APPROVE, model: "claude-opus-5-5" });
  });

  test("a stream the agent printed no answer into reads as no text", () => {
    expect(answerOf('{"type":"agent_start"}\n')).toEqual({ text: "", model: null });
  });
});

describe("a fault detail written to disk", () => {
  /** The variable the scrub reads a credential out of, which the agent's child carries too. */
  const KEY_VARIABLE = "TYPESAFE_API_KEY";

  test("carries no credential, since the detail is the child's own output", async () => {
    const secret = "sk-stand-in-0123456789abcdef";
    const held = process.env[KEY_VARIABLE];
    process.env[KEY_VARIABLE] = secret;
    try {
      const out = await scratch();
      const deps: FreshDeps = {
        ask: () =>
          Promise.resolve({
            text: "",
            model: "fake-opus",
            latencyMs: 7,
            failure: `the child exited 1: GET /v1/messages\nauthorization: Bearer ${secret}`,
          }),
        now: () => Date.parse("2026-11-12T09:00:00Z"),
        log: () => {},
      };
      await runFresh(
        {
          neighbours: "with",
          cases: [freshCase("case-0")],
          out,
          homes: { "with-skill": "/home-with", "no-skill": "/home-no" },
          concurrency: 2,
        },
        deps
      );
      const written = await readFile(answersPath(out, "with"), "utf8");
      expect(written).toContain("the child exited 1");
      expect(written).toContain("<redacted>");
      expect(written).not.toContain(secret);
    } finally {
      if (held === undefined) delete process.env[KEY_VARIABLE];
      else process.env[KEY_VARIABLE] = held;
    }
  });
});
