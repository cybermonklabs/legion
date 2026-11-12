import { afterEach, describe, expect, test } from "bun:test";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { CaseKind, CaseRow } from "../build-corpus";
import type { ClaudeAsk, JevAsk, ModelAnswer } from "../models";
import type { ReplayedDocument } from "../replay";
import {
  type Arm,
  ArmsError,
  armsOf,
  assertSameCases,
  type CaseEvidence,
  inSeedOrder,
  type JudgementRow,
  promptOf,
  type RoundDeps,
  type RoundPlan,
  type RoundSummary,
  RowStore,
  runRound,
  type ScoredCase,
  seedOf,
} from "../run-round";

/**
 * The round runner over hand-written cases and a fake endpoint: nothing here reads a corpus, a
 * draw or Dispatch, and nothing makes a call. What it holds the runner to is the four rules the
 * plan and the handoff name - every arm sees the same case list, a resume asks only what has no
 * answered row, a judgement's seed decides its evidence order, and the deadline stops the run
 * cleanly - and the rule that no credential reaches a row, a summary or a printed line.
 */

/** The directories this file made, each removed by its own exact path. */
const made: string[] = [];

afterEach(async () => {
  for (const directory of made.splice(0)) await rm(directory, { recursive: true, force: true });
});

/** One scratch directory under the scratch volume, never under the workspace. */
async function scratch(): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), "run-round-"));
  made.push(directory);
  return directory;
}

/** One case, with only the fields the runner and the rendering read named. */
function caseRow(caseId: string, kind: CaseKind = "approval"): CaseRow {
  return {
    caseId,
    eventId: 1,
    askId: `ask-${caseId}`,
    issueKey: "PROJ-1",
    project: "PROJ",
    kind,
    urgency: "med",
    multiple: false,
    question: `Approve the spec of ${caseId}?`,
    options: [
      { id: "o1", label: "Approve", description: "it is ready" },
      { id: "o2", label: "Request changes", description: "it is not" },
    ],
    selected: ["Approve"],
    words: null,
    hasWords: false,
    askCreatedAt: "2026-11-06T09:00:00Z",
    answeredAt: "2026-11-06T12:00:00Z",
    eventCreatedAt: "2026-11-06T12:00:00Z",
    approval:
      kind === "approval"
        ? {
            documents: [
              { artifactId: `${caseId}-a`, name: "spec.md", version: 3, requestedVersion: 3 },
              { artifactId: `${caseId}-b`, name: "plan.md", version: 2, requestedVersion: 2 },
              { artifactId: `${caseId}-c`, name: "notes.md", version: 1, requestedVersion: 1 },
            ],
            artifactSlug: "spec",
            artifactPrimary: true,
          }
        : null,
    verdict: kind === "approval" ? "approve" : null,
    confounds: {
      version: 3,
      answersThatDayBefore: 0,
      hourUtc: 12,
      hourPacific: 4,
      changesAlreadyRequested: false,
      priorCasesOnDocument: 0,
      caseKind: kind,
    },
  };
}

/** One document of a case, as `replayCase` hands it over. */
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

/** The evidence of one case: three documents and eight neighbours, built once by the runner. */
function evidenceOf(scored: ScoredCase): CaseEvidence {
  const id = scored.row.caseId;
  return {
    documents: [
      document("spec.md", `# The spec of ${id}\n\nIt says what it does.`, 3),
      document("plan.md", `# The plan of ${id}\n\nIt says when.`, 2),
      document("notes.md", `# The notes of ${id}\n\nThey say why.`, 1),
    ],
    issueTitle: `The issue of ${id}`,
    nearest: {
      neighbours: Array.from({ length: 8 }, (_, at) => ({
        row: caseRow(`neighbour-${at}`),
        place: at + 1,
        score: 8 - at,
      })),
      route: "search",
      excluded: 2,
      degraded: null,
      allowed: 40,
      pages: 1,
    },
  };
}

/** The two arms these tests score, one on each endpoint, neither needing a distillation. */
const ARMS_UNDER_TEST: Arm[] = [
  { name: "bare-one", recipe: "bare", model: "fake-claude", endpoint: "claude", slices: ["fit"] },
  {
    name: "many-shot-two",
    recipe: "many-shot",
    model: "fake-jev",
    endpoint: "jev",
    slices: ["fit"],
  },
];

/** Three cases of the fit slice, which is the list both arms see. */
function cases(count = 3): ScoredCase[] {
  return Array.from({ length: count }, (_, at) => ({
    row: caseRow(`case-${at}`),
    slice: "fit" as const,
    kind: "approval" as const,
  }));
}

/** One answer of a fake endpoint, as the round records it. */
function answer(chosen: string[], fault: ModelAnswer["fault"] = null, detail = ""): ModelAnswer {
  return {
    chosen,
    probabilities: fault === null ? { Approve: 0.8, "Request changes": 0.2 } : null,
    confidence: fault === null ? 0.8 : null,
    inputTokens: 120,
    outputTokens: 7,
    latencyMs: 12,
    attempts: fault === null ? 1 : 5,
    stateCut: false,
    fault,
    faultDetail: fault === null ? null : detail,
  };
}

/** What one fake run saw and said, so a test can read the prompts and the printed lines. */
interface Fake extends RoundDeps {
  asks: { arm: string; model: string; system: string; user: string }[];
  lines: string[];
  clock: number;
}

/** A round's world, with the clock the caller moves and the answer the caller chooses. */
function fake(
  answerOf: (ask: { model: string; user: string }, at: number) => ModelAnswer,
  stepMs = 0
): Fake {
  const deps: Fake = {
    asks: [],
    lines: [],
    clock: Date.parse("2026-11-10T09:00:00Z"),
    evidenceOf: async (scored: ScoredCase) => evidenceOf(scored),
    askClaude: async (ask: ClaudeAsk) => {
      deps.asks.push({
        arm: "claude",
        model: ask.model,
        system: ask.prompt.system,
        user: ask.prompt.user,
      });
      deps.clock += stepMs;
      return answerOf({ model: ask.model, user: ask.prompt.user }, deps.asks.length - 1);
    },
    askJev: async (ask: JevAsk) => {
      deps.asks.push({
        arm: "jev",
        model: ask.model,
        system: ask.options.instructions,
        user: ask.state,
      });
      deps.clock += stepMs;
      return answerOf({ model: ask.model, user: ask.state }, deps.asks.length - 1);
    },
    now: () => deps.clock,
    log: (line: string) => {
      deps.lines.push(line);
    },
  };
  return deps;
}

/** The plan these tests run, over one scratch directory. */
function planOf(out: string, over: Partial<RoundPlan> = {}): RoundPlan {
  return {
    out,
    arms: ARMS_UNDER_TEST,
    cases: cases(),
    judgements: 5,
    concurrency: 2,
    deadlineSeconds: null,
    distillationPath: null,
    probe: false,
    probeOnly: false,
    ...over,
  };
}

/** The rows of a finished run, read back from the file the runner wrote. */
async function rowsOf(out: string): Promise<JudgementRow[]> {
  const text = await readFile(join(out, "rows.jsonl"), "utf8");
  return text
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => JSON.parse(line) as JudgementRow);
}

/** The summary of a finished run, read back from `round.json`. */
async function summaryOf(out: string): Promise<RoundSummary> {
  return JSON.parse(await readFile(join(out, "round.json"), "utf8")) as RoundSummary;
}

describe("the arms must see the same cases", () => {
  test("two arms over different slices are refused, and both are named", () => {
    const arms = armsOf(
      [
        { name: "bare-one", recipe: "bare", model: "m", endpoint: "claude", slices: ["fit"] },
        {
          name: "bare-two",
          recipe: "bare",
          model: "m",
          endpoint: "claude",
          slices: ["fit", "report"],
        },
      ],
      "arms.json"
    );
    const over = [
      ...cases(2),
      { row: caseRow("case-report"), slice: "report" as const, kind: "approval" as const },
    ];
    expect(() => {
      assertSameCases(arms, over);
    }).toThrow(ArmsError);
    try {
      assertSameCases(arms, over);
      throw new Error("the refusal did not happen");
    } catch (error) {
      const message = (error as Error).message;
      expect(message).toContain("bare-one");
      expect(message).toContain("bare-two");
      expect(message).toContain("different case lists");
    }
  });

  test("a round over those arms refuses before any arm is asked", async () => {
    const out = await scratch();
    const deps = fake(() => answer(["Approve"]));
    const arms: Arm[] = [
      { ...(ARMS_UNDER_TEST[0] as Arm), slices: ["fit"] },
      { ...(ARMS_UNDER_TEST[1] as Arm), slices: ["fit", "anchor"] },
    ];
    const over = [
      ...cases(1),
      { row: caseRow("case-anchor"), slice: "anchor" as const, kind: "approval" as const },
    ];
    await expect(runRound(planOf(out, { arms, cases: over }), deps)).rejects.toThrow(ArmsError);
    expect(deps.asks).toHaveLength(0);
  });

  test("an arms file naming an unknown recipe is refused", () => {
    expect(() =>
      armsOf([{ name: "x", recipe: "guesswork", model: "m", endpoint: "claude" }], "arms.json")
    ).toThrow(ArmsError);
  });
});

describe("a resume asks only what has no answered row", () => {
  test("it skips what answered and asks the faulted judgement again, replacing its row", async () => {
    const out = await scratch();
    // One call of the first run is answered with a fault, chosen by its place in the order the
    // round walks, since a fake endpoint sees the prompt and not the key of the judgement.
    const faultAt = 7;
    const firstRun = fake((_ask, at) =>
      at === faultAt ? answer([], "status", "503 the endpoint was out") : answer(["Approve"])
    );
    const planned = planOf(out, { concurrency: 1 });
    const one = await runRound(planned, firstRun);
    expect(firstRun.asks).toHaveLength(30);
    expect(one.summary.judgements.planned).toBe(30);
    expect(one.summary.judgements.answered).toBe(29);
    expect(one.summary.faults.byKind.status?.count).toBe(1);
    expect(one.summary.faults.byKind.status?.example).toBe("503 the endpoint was out");

    const secondRun = fake(() => answer(["Request changes"]));
    const two = await runRound(planned, secondRun);
    expect(secondRun.asks).toHaveLength(1);
    expect(two.summary.judgements.answered).toBe(30);
    expect(two.exitCode).toBe(0);
    const rows = await rowsOf(out);
    expect(rows).toHaveLength(30);
    const keys = new Set(rows.map((row) => `${row.arm}|${row.caseId}|${row.judgement}`));
    expect(keys.size).toBe(30);
    const replaced = rows.filter((row) => row.verdict.join() === "Request changes");
    expect(replaced).toHaveLength(1);
    expect(replaced[0]?.fault).toBeNull();

    const third = fake(() => answer(["Approve"]));
    const threeResult = await runRound(planned, third);
    expect(third.asks).toHaveLength(0);
    expect(threeResult.exitCode).toBe(0);
    expect((await rowsOf(out)).length).toBe(30);
  });
});

describe("a round run one arms file at a time is finished when its stages are", () => {
  test("a later stage is not called unfinished by the rows an earlier stage left", async () => {
    const out = await scratch();
    const [one, two] = ARMS_UNDER_TEST as [Arm, Arm];
    const first = await runRound(
      planOf(out, { arms: [one] }),
      fake(() => answer(["Approve"]))
    );
    expect(first.summary.judgements.planned).toBe(15);
    expect(first.summary.finished).toBe(true);

    // The second stage names its own arm alone, and the first stage's rows are in the store it
    // reads. A round that counts its answered judgements over every arm of the store and its
    // planned ones over this stage's arms reports more answered than planned, calls itself
    // unfinished and exits 10, which is what a round stopped by its deadline reports.
    const second = await runRound(
      planOf(out, { arms: [two] }),
      fake(() => answer(["Approve"]))
    );
    expect(second.summary.judgements.answered).toBeLessThanOrEqual(
      second.summary.judgements.planned
    );
    expect(second.summary.finished).toBe(true);
    expect(second.exitCode).toBe(0);
    expect(await rowsOf(out)).toHaveLength(30);
  });
});

describe("the seed decides the order of the evidence", () => {
  test("one seed orders the same way twice, and other seeds order otherwise", () => {
    const items = ["a", "b", "c", "d", "e", "f", "g", "h"];
    expect(inSeedOrder(items, 4160884415)).toEqual(inSeedOrder(items, 4160884415));
    const orders = new Set([1, 2, 3, 4, 5].map((seed) => inSeedOrder(items, seed).join("")));
    expect(orders.size).toBeGreaterThan(1);
  });

  test("the judgements of one case and arm differ in their order and repeat exactly", async () => {
    const arm = ARMS_UNDER_TEST[1] as Arm;
    const scored = cases(1)[0] as ScoredCase;
    const evidence = evidenceOf(scored);
    const prompts: string[] = [];
    for (let judgement = 0; judgement < 5; judgement += 1) {
      const seed = seedOf(scored.row.caseId, arm.name, judgement);
      const prompt = await promptOf(arm, scored, evidence, seed, null);
      const again = await promptOf(arm, scored, evidence, seed, null);
      expect(prompt.user).toBe(again.user);
      prompts.push(prompt.user);
    }
    expect(new Set(prompts).size).toBeGreaterThan(1);
  });

  test("the row carries the seed its prompt was ordered by", async () => {
    const out = await scratch();
    const deps = fake(() => answer(["Approve"]));
    await runRound(planOf(out, { judgements: 2 }), deps);
    for (const row of await rowsOf(out)) {
      expect(row.seed).toBe(seedOf(row.caseId, row.arm, row.judgement));
    }
  });
});

describe("the deadline stops the run cleanly", () => {
  test("it exits 10, flushes what it has and prints what is left", async () => {
    const out = await scratch();
    const deps = fake(() => answer(["Approve"]), 1_000);
    const result = await runRound(planOf(out, { concurrency: 1, deadlineSeconds: 2 }), deps);
    expect(result.exitCode).toBe(10);
    expect(deps.asks).toHaveLength(2);
    expect(result.summary.finished).toBe(false);
    expect(result.summary.judgements.answered).toBe(2);
    expect(result.summary.judgements.left).toBe(28);
    expect(await rowsOf(out)).toHaveLength(2);
    const left = deps.lines.filter((line) => line.includes("left"));
    expect(left.some((line) => line.includes("deadline"))).toBe(true);
    expect(left.some((line) => line.includes("bare-one"))).toBe(true);
    expect((await summaryOf(out)).judgements.left).toBe(28);
  });
});

describe("no credential reaches a row, a summary or a printed line", () => {
  test("a fault detail holding the key is scrubbed on all three paths", async () => {
    const out = await scratch();
    const standIn = "stand-in-secret-5f4dcc3b5aa765d61d8327deb882cf99";
    const held = process.env.TYPESAFE_API_KEY;
    process.env.TYPESAFE_API_KEY = standIn;
    try {
      const deps = fake(() =>
        answer([], "status", `401 {"error":"bad key","sent":"Bearer ${standIn}"}`)
      );
      const result = await runRound(planOf(out, { judgements: 1 }), deps);
      expect(result.summary.judgements.answered).toBe(0);
      const rows = await readFile(join(out, "rows.jsonl"), "utf8");
      const round = await readFile(join(out, "round.json"), "utf8");
      expect(rows).not.toContain(standIn);
      expect(round).not.toContain(standIn);
      expect(deps.lines.join("\n")).not.toContain(standIn);
      expect(rows).toContain("<redacted>");
      expect(round).toContain("<redacted>");
      const printed = deps.lines.filter((line) => line.startsWith("fault "));
      expect(printed).toHaveLength(1);
      expect(printed[0]).toContain("401");
      expect(printed[0]).toContain("<redacted>");
      expect(result.summary.faults.byArm["bare-one"]?.example).toContain("<redacted>");
    } finally {
      if (held === undefined) delete process.env.TYPESAFE_API_KEY;
      else process.env.TYPESAFE_API_KEY = held;
    }
  });
});

describe("the round refuses a case whose rendering keeps the answer", () => {
  test("an approval of the requested version stops the round, naming the case", async () => {
    const out = await scratch();
    const deps = fake(() => answer(["Approve"]));
    const leaking: RoundDeps = {
      ...deps,
      evidenceOf: async () => ({
        documents: [
          document(
            "spec.md",
            "# The spec\n\nApproval: approved v3 — it went in.\n\nIt says what it does.",
            3
          ),
        ],
        issueTitle: "The issue",
        nearest: null,
      }),
    };
    await expect(
      runRound(planOf(out, { arms: [ARMS_UNDER_TEST[0] as Arm], cases: cases(1) }), leaking)
    ).rejects.toThrow(/case-0/);
    expect(deps.asks).toHaveLength(0);
  });
});

describe("the rows file", () => {
  /** One row of a judgement, with only the fields the store keys and reads named. */
  function judgementRow(caseId: string, judgement: number, fault: string | null): JudgementRow {
    return {
      arm: "bare-opus",
      recipe: "bare",
      model: "a-model",
      endpoint: "claude",
      caseId,
      slice: "report",
      kind: "approval",
      judgement,
      seed: 1,
      verdict: fault === null ? ["Approve"] : [],
      probabilities: null,
      confidence: null,
      inputTokens: 1,
      outputTokens: 1,
      latencyMs: 1,
      attempts: 1,
      stateCut: false,
      retrievalRoute: null,
      examples: 0,
      excluded: null,
      promptBytes: 1,
      fault: fault as JudgementRow["fault"],
      faultDetail: fault === null ? null : "the endpoint closed the connection",
      at: "2026-11-12T00:00:00Z",
    };
  }

  test("holds one row per key, however many rewrites are queued at one moment", async () => {
    const path = join(await scratch(), "rows.jsonl");
    const store = await RowStore.open(path);
    for (let judgement = 0; judgement < 3; judgement += 1) {
      await store.put(judgementRow("case-0", judgement, "transport"));
    }
    // Three replacements asked for together: the rewrite queued for the first serializes the
    // map the other two are already in, and a second rewrite would write the same bytes again.
    await Promise.all([
      store.put(judgementRow("case-0", 0, null)),
      store.put(judgementRow("case-0", 1, null)),
      store.put(judgementRow("case-0", 2, null)),
    ]);
    const written = (await readFile(path, "utf8")).trim().split("\n");
    expect(written).toHaveLength(3);
    const rows = written.map((line) => JSON.parse(line) as JudgementRow);
    expect(rows.map((row) => row.judgement)).toEqual([0, 1, 2]);
    expect(rows.every((row) => row.fault === null)).toBe(true);
    const again = await RowStore.open(path);
    expect(again.all()).toHaveLength(3);
  });

  test("a replacement arriving on its own rewrites the file at once", async () => {
    const path = join(await scratch(), "rows.jsonl");
    const store = await RowStore.open(path);
    await store.put(judgementRow("case-0", 0, "transport"));
    await store.put(judgementRow("case-0", 0, null));
    const written = (await readFile(path, "utf8")).trim().split("\n");
    expect(written).toHaveLength(1);
    expect((JSON.parse(written[0] as string) as JudgementRow).fault).toBeNull();
    expect(store.answered("bare-opus\u0000case-0\u00000")).toBe(true);
  });
});
