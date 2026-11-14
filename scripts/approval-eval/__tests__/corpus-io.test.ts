import { describe, expect, test } from "bun:test";
import type { ApprovalDocument, CaseRow } from "../build-corpus";
import {
  answeredTime,
  artifactsOf,
  CannotRunError,
  CorpusError,
  cutTime,
  labelsOfCase,
  leadDocumentOf,
  num,
  OutputPathError,
  signed,
  signedBound,
  span,
  UsageError,
  whole,
} from "../corpus-io";
import {
  DispatchConfigError,
  DispatchRefusedError,
  DispatchUnreachableError,
} from "../dispatch-read";
import { DrawError } from "../draw";
import { FreshError } from "../fresh-agent";
import { GatewayError, JevKeyError } from "../models";
import { PoolError } from "../pool-runs";
import { RecipeError } from "../recipes";
import { LeakError } from "../render";
import { ArmsError, ProbeError, RoundError } from "../run-round";

/**
 * The helpers every command of this directory shares, held at their one home.
 *
 * Each case here was two or three functions before, in two or three files, and what it asserts
 * is the reading that was kept: the formatter that prints to the places it is given, the flag
 * readers that carry their own command's usage, the fields a case is read by, and the one base
 * class that says a command cannot run.
 */

/** One approval case, with only the fields these helpers read named. */
function caseRow(overrides: Partial<CaseRow> = {}): CaseRow {
  return {
    caseId: "case-1",
    eventId: 1,
    askId: "ask-1",
    issueKey: "PROJ-1",
    project: "PROJ",
    kind: "approval",
    urgency: "med",
    multiple: false,
    question: "Approve spec.md (version 7)?",
    options: [
      { id: "b2", label: "Approve", description: "Approve this version of the document." },
      { id: "b3", label: "Request changes", description: "Say what must change." },
    ],
    selected: ["Approve"],
    words: null,
    hasWords: false,
    askCreatedAt: "2026-11-06T09:00:00Z",
    answeredAt: "2026-11-06T12:00:00Z",
    eventCreatedAt: "2026-11-06T12:00:00Z",
    approval: {
      documents: [{ artifactId: "artifact-1", name: "spec.md", version: 7, requestedVersion: 7 }],
      artifactSlug: "spec",
      artifactPrimary: true,
    },
    verdict: "approve",
    confounds: {
      version: 7,
      answersThatDayBefore: 0,
      hourUtc: 12,
      hourPacific: 4,
      changesAlreadyRequested: false,
      priorCasesOnDocument: 0,
      caseKind: "approval",
    },
    ...overrides,
  };
}

/** One approval case over the documents given, in the order the request named them. */
function overDocuments(documents: readonly ApprovalDocument[]): CaseRow {
  return caseRow({
    approval: { documents: [...documents], artifactSlug: null, artifactPrimary: null },
  });
}

describe("num", () => {
  test("prints a share to three places, which is what every record carries", () => {
    expect(num(0.9375)).toBe("0.938");
    expect(num(0)).toBe("0.000");
    expect(num(1)).toBe("1.000");
  });

  test("prints to the places the caller names, which no share needs", () => {
    expect(num(100 * 0.05, 0)).toBe("5");
    expect(num(1.234_56, 4)).toBe("1.2346");
    expect(num(0.95, 5)).toBe("0.95000");
  });

  test("is the precision signed and span print at", () => {
    expect(signed(0.012_3)).toBe(`+${num(0.012_3)}`);
    expect(span({ lower: -0.01, upper: 0.02 })).toBe(
      `[${signedBound(-0.01)}, ${signedBound(0.02)}]`
    );
  });

  /**
   * An estimate and a bound round the same way and take their signs by different rules: two
   * estimates equal at this precision may not read as a rise and a fall, while a bound below
   * zero says the interval reaches below zero however small it is.
   */
  test("keeps the sign of a bound that rounds to zero, and not of such an estimate", () => {
    expect(signed(-0.000_02)).toBe("+0.000");
    expect(signedBound(-0.000_02)).toBe("−0.000");
    expect(signedBound(0)).toBe("+0.000");
    expect(span({ lower: -0.000_02, upper: 0.086_737 })).toBe("[−0.000, +0.087]");
  });
});

describe("whole", () => {
  test("takes the fallback when the flag was not named", () => {
    expect(whole(undefined, "--k", 5, 1, "usage")).toBe(5);
  });

  test("reads a whole number at or above the least", () => {
    expect(whole(" 12 ", "--k", 5, 1, "usage")).toBe(12);
    expect(whole("0", "--seed", 7, 0, "usage")).toBe(0);
  });

  test("refuses a number below the least, naming the flag and the caller's usage", () => {
    expect(() => whole("0", "--k", 5, 1, "the usage of this command")).toThrow(UsageError);
    expect(() => whole("0", "--k", 5, 1, "the usage of this command")).toThrow(
      "--k takes a whole number of 1 or above\n\nthe usage of this command"
    );
  });

  test("refuses what is no whole number", () => {
    for (const value of ["1.5", "ten", "", "1e400"]) {
      expect(() => whole(value, "--cases", 5, 1, "usage")).toThrow(UsageError);
    }
  });
});

describe("cutTime", () => {
  test("reads an ISO 8601 cut", () => {
    expect(cutTime("2026-11-06T12:00:00Z")).toBe(Date.parse("2026-11-06T12:00:00Z"));
  });

  test("takes a cut a caller already holds as a date", () => {
    const at = new Date("2026-11-06T12:00:00Z");
    expect(cutTime(at)).toBe(cutTime("2026-11-06T12:00:00Z"));
  });

  test("refuses a cut that is no time, naming the flag", () => {
    expect(() => cutTime("the sixth")).toThrow(UsageError);
    expect(() => cutTime("the sixth")).toThrow("--cut takes an ISO 8601 time, and the sixth");
  });
});

describe("answeredTime", () => {
  test("reads when the case was answered", () => {
    expect(answeredTime(caseRow())).toBe(Date.parse("2026-11-06T12:00:00Z"));
  });

  test("refuses a stamp that is no time, naming the case", () => {
    const row = caseRow({ caseId: "case-9", answeredAt: "whenever" });
    expect(() => answeredTime(row)).toThrow(CorpusError);
    expect(() => answeredTime(row)).toThrow(
      "case case-9 was answered at whenever, which is no time"
    );
  });
});

describe("artifactsOf", () => {
  test("names every artifact the request carries, each once", () => {
    const row = overDocuments([
      { artifactId: "artifact-1", name: "spec.md", version: 7, requestedVersion: 7 },
      { artifactId: "artifact-1", name: "spec.md", version: 8, requestedVersion: 8 },
      { artifactId: "artifact-2", name: "plan.md", version: 2, requestedVersion: 2 },
    ]);
    expect([...artifactsOf(row)]).toEqual(["artifact-1", "artifact-2"]);
  });

  test("passes over a document naming no artifact", () => {
    const row = overDocuments([
      { artifactId: null, name: "spec.md", version: 7, requestedVersion: 7 },
      { artifactId: "artifact-2", name: "plan.md", version: 2, requestedVersion: 2 },
    ]);
    expect([...artifactsOf(row)]).toEqual(["artifact-2"]);
  });

  test("names nothing for a case that asks to approve nothing", () => {
    expect([...artifactsOf(caseRow({ approval: null }))]).toEqual([]);
  });
});

describe("labelsOfCase", () => {
  test("names the labels the case offered, trimmed and each once", () => {
    const row = caseRow({
      options: [
        { id: "b1", label: " Approve ", description: null },
        { id: "b2", label: "Approve", description: null },
        { id: "b3", label: "Request changes", description: null },
      ],
    });
    expect(labelsOfCase(row)).toEqual(["Approve", "Request changes"]);
  });

  test("passes over an option with no label", () => {
    const row = caseRow({
      options: [
        { id: "b1", label: null, description: null },
        { id: "b2", label: "   ", description: null },
        { id: "b3", label: "Approve", description: null },
      ],
    });
    expect(labelsOfCase(row)).toEqual(["Approve"]);
  });
});

describe("leadDocumentOf", () => {
  test("leads with the first document naming both an artifact and a version", () => {
    expect(leadDocumentOf(caseRow())).toEqual({ artifactId: "artifact-1", requestedVersion: 7 });
  });

  /**
   * `analyze.ts` counts distinct documents by this one, and nothing else reads it. A document
   * naming a version but no artifact names no document to count by, so this passes over it.
   * The draw records the first document the judge was shown, which it finds among the replayed
   * documents, and the leak guard of `render.ts` holds each document's section of a rendering
   * to that document's own requested version; neither goes through this.
   */
  test("passes over a document naming a version but no artifact", () => {
    const row = overDocuments([
      { artifactId: null, name: "notes.md", version: 9, requestedVersion: 9 },
      { artifactId: "artifact-2", name: "spec.md", version: 3, requestedVersion: 3 },
    ]);
    expect(leadDocumentOf(row)).toEqual({ artifactId: "artifact-2", requestedVersion: 3 });
  });

  test("passes over a document naming an artifact but no version", () => {
    const row = overDocuments([
      { artifactId: "artifact-1", name: "spec.md", version: null, requestedVersion: null },
      { artifactId: "artifact-2", name: "plan.md", version: 2, requestedVersion: 2 },
    ]);
    expect(leadDocumentOf(row)).toEqual({ artifactId: "artifact-2", requestedVersion: 2 });
  });

  test("leads with nothing where no document names both", () => {
    expect(leadDocumentOf(caseRow({ approval: null }))).toBeNull();
    expect(
      leadDocumentOf(
        overDocuments([{ artifactId: null, name: "spec.md", version: 7, requestedVersion: 7 }])
      )
    ).toBeNull();
  });
});

describe("CannotRunError", () => {
  /**
   * Every class a command's `main` is expected to answer for, including the four that one chain
   * or another did not list: a round's `RecipeError` and `GatewayError`, which fresh-agent.ts
   * and draw.ts could both raise and neither named, and `CorpusError` and `UsageError`, which
   * build-corpus.ts did not name. A class of this family added to one command is caught by all
   * of them, so no chain can be left short again.
   */
  const expected: readonly [string, CannotRunError][] = [
    ["UsageError", new UsageError("flags")],
    ["OutputPathError", new OutputPathError("out")],
    ["CorpusError", new CorpusError("corpus")],
    ["DispatchConfigError", new DispatchConfigError("config")],
    ["DispatchRefusedError", new DispatchRefusedError("/api/v1/issues", 404, "not found")],
    ["DispatchUnreachableError", new DispatchUnreachableError("/api/v1/issues", 3, "timeout")],
    ["DrawError", new DrawError("draw")],
    ["FreshError", new FreshError("fresh")],
    ["GatewayError", new GatewayError("gateway")],
    ["JevKeyError", new JevKeyError()],
    ["PoolError", new PoolError("pool")],
    ["RecipeError", new RecipeError("recipe")],
    ["LeakError", new LeakError("case-1", "Approval: approved v7", "an approval")],
    ["ArmsError", new ArmsError("arms")],
    ["RoundError", new RoundError("round")],
  ];

  for (const [name, error] of expected) {
    test(`catches ${name}, whichever command raises it`, () => {
      expect(error).toBeInstanceOf(CannotRunError);
      expect(error.name).toBe(name);
    });
  }

  /** The probe takes an exit status of its own, so it is answered before this class and not by it. */
  test("does not catch ProbeError, which has its own exit status", () => {
    expect(new ProbeError("probe")).not.toBeInstanceOf(CannotRunError);
  });

  test("leaves an error of no expected class to throw", () => {
    expect(new RangeError("a number out of range")).not.toBeInstanceOf(CannotRunError);
    expect(new Error("anything else")).not.toBeInstanceOf(CannotRunError);
  });
});
