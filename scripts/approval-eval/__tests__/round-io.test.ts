import { afterEach, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { basename, join } from "node:path";
import {
  CACHE_VERSION,
  type CachedEvidence,
  cachePath,
  evidenceDirectory,
  type JudgementRow,
  readEvidenceCache,
  readEvidenceNeighbours,
  readRows,
  rowOf,
  rowsPath,
} from "../round-io";

/**
 * The owner of a round's two files, read as the four commands that read them do.
 *
 * What it holds the module to is the two rules that were two rules before it: one line is a row
 * of `rows.jsonl` or it is not, whoever is reading, and a round with no `rows.jsonl` is a round
 * that has written no row rather than a failure for some readers and nothing for others.
 */

/** The directories this file made, each removed by its own exact path. */
const made: string[] = [];

afterEach(async () => {
  for (const directory of made.splice(0)) await rm(directory, { recursive: true, force: true });
});

/** One scratch directory under the scratch volume, never under the workspace. */
async function scratch(): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), "round-io-"));
  made.push(directory);
  return directory;
}

/** One row as the runner writes it. */
function judgementRow(overrides: Partial<JudgementRow> = {}): JudgementRow {
  return {
    arm: "bare-opus-5-5",
    recipe: "bare",
    model: "anthropic/claude-opus-5-5",
    endpoint: "claude",
    caseId: "case-a",
    slice: "report",
    kind: "approval",
    judgement: 0,
    seed: 1,
    verdict: ["Approve"],
    probabilities: null,
    confidence: null,
    inputTokens: 10,
    outputTokens: 5,
    latencyMs: 100,
    attempts: 1,
    stateCut: false,
    retrievalRoute: null,
    examples: 0,
    excluded: null,
    promptBytes: 500,
    fault: null,
    faultDetail: null,
    at: "2026-11-12T09:00:00.000Z",
    ...overrides,
  };
}

/** One cache entry as the runner writes it. */
function cachedEvidence(overrides: Partial<CachedEvidence> = {}): CachedEvidence {
  return {
    version: CACHE_VERSION,
    caseId: "case-a",
    builtAt: "2026-11-12T09:00:00.000Z",
    issueTitle: "An issue",
    documents: [],
    examples: 2,
    nearest: {
      route: "search",
      excluded: 3,
      degraded: null,
      allowed: 10,
      pages: 1,
      neighbours: [{ caseId: "case-b", place: 0, score: 0.5 }],
    },
    ...overrides,
  };
}

describe("rowOf", () => {
  test("reads one row the runner wrote", () => {
    const row = judgementRow();
    expect(rowOf(JSON.stringify(row))).toEqual(row);
  });

  test("refuses a row whose verdict is no list, which every reading of a row takes as one", () => {
    const line = JSON.stringify({ ...judgementRow(), verdict: "Approve" });
    expect(rowOf(line)).toBeNull();
  });

  test("refuses a line that is no JSON object, and one naming no arm, case or judgement", () => {
    expect(rowOf("{")).toBeNull();
    expect(rowOf("[]")).toBeNull();
    expect(rowOf(JSON.stringify({ ...judgementRow(), arm: 3 }))).toBeNull();
    expect(rowOf(JSON.stringify({ ...judgementRow(), caseId: null }))).toBeNull();
    expect(rowOf(JSON.stringify({ ...judgementRow(), judgement: "0" }))).toBeNull();
  });
});

describe("readRows", () => {
  test("reads a round with no rows.jsonl as a round that has written no row", async () => {
    const round = await scratch();
    expect(await readRows(round)).toEqual({ rows: [], read: 0, unreadable: 0 });
  });

  test("reads the rows that are there and counts the lines that are no row", async () => {
    const round = await scratch();
    const lines = [
      JSON.stringify(judgementRow()),
      "",
      "{not json",
      JSON.stringify({ ...judgementRow(), judgement: 1, verdict: "Approve" }),
      JSON.stringify(judgementRow({ judgement: 2 })),
    ];
    await writeFile(rowsPath(round), `${lines.join("\n")}\n`, "utf8");
    const held = await readRows(round);
    expect(held.rows.map((row) => row.judgement)).toEqual([0, 2]);
    expect(held.read).toBe(4);
    expect(held.unreadable).toBe(2);
  });
});

describe("readEvidenceCache", () => {
  test("says which miss it was, so a caller can build it again or refuse the round", async () => {
    const round = await scratch();
    expect(await readEvidenceCache(round, "case-a")).toEqual({ cache: null, miss: "no file" });
    await mkdir(evidenceDirectory(round), { recursive: true });
    await writeFile(
      cachePath(round, "case-a"),
      `${JSON.stringify(cachedEvidence({ version: CACHE_VERSION + 1 }))}\n`,
      "utf8"
    );
    expect(await readEvidenceCache(round, "case-a")).toEqual({
      cache: null,
      miss: "another case or version",
    });
  });

  test("reads one case's evidence at this version", async () => {
    const round = await scratch();
    const cache = cachedEvidence();
    await mkdir(evidenceDirectory(round), { recursive: true });
    await writeFile(cachePath(round, "case-a"), `${JSON.stringify(cache)}\n`, "utf8");
    expect(await readEvidenceCache(round, "case-a")).toEqual({ cache, miss: null });
  });

  test("is a miss where the entry names another case, whatever its file is called", async () => {
    const round = await scratch();
    await mkdir(evidenceDirectory(round), { recursive: true });
    await writeFile(
      cachePath(round, "case-a"),
      `${JSON.stringify(cachedEvidence({ caseId: "case-b" }))}\n`,
      "utf8"
    );
    expect(await readEvidenceCache(round, "case-a")).toEqual({
      cache: null,
      miss: "another case or version",
    });
  });
});

describe("readEvidenceNeighbours", () => {
  test("reads a round with no cache at all as no entries", async () => {
    expect(await readEvidenceNeighbours(await scratch())).toEqual({ cases: [], unreadable: 0 });
  });

  test("reads the entries that are there and counts what is no entry", async () => {
    const round = await scratch();
    await mkdir(evidenceDirectory(round), { recursive: true });
    await writeFile(cachePath(round, "case-a"), `${JSON.stringify(cachedEvidence())}\n`, "utf8");
    await writeFile(join(evidenceDirectory(round), "broken.json"), "{not json", "utf8");
    expect(await readEvidenceNeighbours(round)).toEqual({
      cases: [{ caseId: "case-a", neighbours: ["case-b"], excluded: 3 }],
      unreadable: 1,
    });
  });

  /**
   * The scan the retrieval proof is read over checks the version and the fields of every entry
   * it walks, which the reading it replaced did not: that one took any JSON object naming a
   * case and counted it, with whatever fields it had. Over the recorded round nothing moved,
   * every entry of it being of this version and whole, so this is where the direction that did
   * move is held: an entry of an older version and an entry whose writing never reached the
   * documents are each one case and its neighbours left out of the proof's counts, and each is
   * counted so that the proof can say its numbers are not over the whole cache.
   */
  test("counts an entry of an older version and one the writing never finished", async () => {
    const round = await scratch();
    await mkdir(evidenceDirectory(round), { recursive: true });
    await writeFile(cachePath(round, "case-a"), `${JSON.stringify(cachedEvidence())}\n`, "utf8");
    await writeFile(
      cachePath(round, "case-b"),
      `${JSON.stringify(cachedEvidence({ caseId: "case-b", version: CACHE_VERSION - 1 }))}\n`,
      "utf8"
    );
    const whole = cachedEvidence({ caseId: "case-c" });
    await writeFile(
      cachePath(round, "case-c"),
      `${JSON.stringify({
        version: whole.version,
        caseId: whole.caseId,
        builtAt: whole.builtAt,
        issueTitle: whole.issueTitle,
        nearest: whole.nearest,
      })}\n`,
      "utf8"
    );
    expect(await readEvidenceNeighbours(round)).toEqual({
      cases: [{ caseId: "case-a", neighbours: ["case-b"], excluded: 3 }],
      unreadable: 2,
    });
  });

  /** One file per case, read many at a time, and handed back in the order of their names. */
  test("holds the order of the directory's own names, whatever order the reads answer in", async () => {
    const round = await scratch();
    await mkdir(evidenceDirectory(round), { recursive: true });
    const ids = ["case-a", "case-b", "case-c", "case-d", "case-e"];
    for (const caseId of ids) {
      await writeFile(
        cachePath(round, caseId),
        `${JSON.stringify(cachedEvidence({ caseId }))}\n`,
        "utf8"
      );
    }
    const held = await readEvidenceNeighbours(round);
    const byName = ids
      .map((caseId) => ({ caseId, name: basename(cachePath(round, caseId)) }))
      .sort((one, two) => (one.name < two.name ? -1 : 1))
      .map((one) => one.caseId);
    expect(held.cases.map((one) => one.caseId)).toEqual(byName);
    expect(held.unreadable).toBe(0);
  });
});
