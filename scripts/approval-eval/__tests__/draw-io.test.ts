import { afterEach, describe, expect, test } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { CorpusError } from "../corpus-io";
import {
  corpusDrawPath,
  corpusSlicesPath,
  cutOf,
  type DrawRecord,
  drawPath,
  groupKeyOf,
  issueFactsOf,
  readDraw,
  readSlices,
  rootFactsOf,
  slicesPath,
} from "../draw-io";

/**
 * The owner of the two files one draw writes, read as the four commands that read them do.
 *
 * What it holds the module to is the one policy it chose for both: a directory with no draw
 * carries none, and a file that is there and is not what a draw writes stops the reading
 * rather than being read as a corpus that was never drawn. The facts of a draw are read one by
 * one, because a draw made before a field was recorded carries no such field and is still a
 * draw.
 */

/** The directories this file made, each removed by its own exact path. */
const made: string[] = [];

afterEach(async () => {
  for (const directory of made.splice(0)) await rm(directory, { recursive: true, force: true });
});

/** One scratch directory under the scratch volume, never under the workspace. */
async function scratch(): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), "draw-io-"));
  made.push(directory);
  return directory;
}

/** A draw's record, of the fields a reader of one reads. */
function draw(overrides: Partial<DrawRecord> = {}): DrawRecord {
  return {
    corpus: "/corpus",
    cut: "2026-10-01T00:00:00.000Z",
    seed: 7,
    targets: {
      approval: { fit: 1, report: 1, anchor: 1 },
      question: { fit: 1, report: 1, anchor: 1 },
    },
    counts: {
      fit: { approval: 1, question: 0 },
      report: { approval: 1, question: 0 },
      anchor: { approval: 1, question: 0 },
      reserve: { approval: 0, question: 0 },
    },
    groupKey: "issue",
    groupCount: 3,
    groups: { approval: 3, question: 0 },
    postCutCases: 3,
    documentsUnread: 0,
    backlogLegionRootsRead: 1,
    backlogLegionRoots: {
      readAt: "2026-10-01T00:00:00.000Z",
      source: "dispatch",
      issues: [
        {
          key: "INFRA-1",
          parent: null,
          labels: ["legion"],
          status: "backlog",
          primaryArtifactId: "a",
        },
      ],
    },
    rule: {
      model: "anthropic/claude-opus-5-5",
      definitionSha256: "abc",
      cache: null,
      asked: 1,
      cached: 0,
      inputTokens: 10,
      outputTokens: 5,
    },
    excluded: {
      held: { total: 0, byKind: {}, cases: [] },
      undrawable: {
        total: 0,
        byKind: { "too-few-options": 0, "no-option-chosen": 0, "rule-unreadable": 0 },
        cases: [],
      },
    },
    ...overrides,
  };
}

describe("readDraw", () => {
  test("reads a directory holding no draw as no draw", async () => {
    expect(await readDraw(drawPath(await scratch()))).toBeNull();
  });

  test("stops the reading where a draw is there and is not the record a draw writes", async () => {
    const directory = await scratch();
    const path = drawPath(directory);
    await writeFile(path, "{not json", "utf8");
    await expect(readDraw(path)).rejects.toThrow(CorpusError);
    await expect(readDraw(path)).rejects.toThrow(`${path} is not JSON`);
    await writeFile(path, "[]", "utf8");
    await expect(readDraw(path)).rejects.toThrow(`${path} is not a draw`);
  });

  test("reads the record the draw wrote, under the corpus's own slices directory", async () => {
    const corpus = await scratch();
    await writeFile(drawPath(corpus), `${JSON.stringify(draw())}\n`, "utf8");
    expect(corpusDrawPath(corpus)).toBe(join(corpus, "slices", "draw.json"));
    expect((await readDraw(drawPath(corpus)))?.seed).toBe(7);
  });
});

describe("readSlices", () => {
  test("reads a directory holding no slices as none", async () => {
    expect(await readSlices(slicesPath(await scratch()))).toBeNull();
  });

  test("stops the reading where the file is not the case ids a draw writes", async () => {
    const directory = await scratch();
    const path = slicesPath(directory);
    await writeFile(path, "{not json", "utf8");
    await expect(readSlices(path)).rejects.toThrow(CorpusError);
    await expect(readSlices(path)).rejects.toThrow(`${path} is not JSON`);
    await writeFile(path, "[]", "utf8");
    await expect(readSlices(path)).rejects.toThrow(`${path} is not a draw's slices`);
    await writeFile(path, JSON.stringify({ fit: 3 }), "utf8");
    await expect(readSlices(path)).rejects.toThrow(
      `${path} holds no list of case ids in the fit slice`
    );
    await writeFile(path, JSON.stringify({ fit: ["case-a", 3] }), "utf8");
    await expect(readSlices(path)).rejects.toThrow(
      `${path} names a case that is no string in the fit slice`
    );
  });

  test("reads the ids of every slice, under the corpus's own slices directory", async () => {
    const corpus = await scratch();
    await writeFile(
      slicesPath(corpus),
      `${JSON.stringify({ fit: ["case-a"], report: ["case-b", "case-c"], anchor: [] })}\n`,
      "utf8"
    );
    expect(corpusSlicesPath(corpus)).toBe(join(corpus, "slices", "slices.json"));
    expect(await readSlices(slicesPath(corpus))).toEqual({
      fit: ["case-a"],
      report: ["case-b", "case-c"],
      anchor: [],
      // A slice the file names no case under holds none, as a slice named empty does.
      reserve: [],
    });
  });
});

describe("the facts a reader takes out of a draw", () => {
  test("reads the cut, and no cut from a draw naming none or naming no time", () => {
    expect(cutOf(draw())).toBe("2026-10-01T00:00:00.000Z");
    expect(cutOf(draw({ cut: "the first of October" }))).toBeNull();
    expect(cutOf(null)).toBeNull();
  });

  test("reads no group key from a draw made before that key was recorded", () => {
    const { groupKey, ...before } = draw();
    expect(groupKey).toBe("issue");
    expect(groupKeyOf(before as DrawRecord)).toBeNull();
    expect(groupKeyOf(draw())).toBe("issue");
    expect(groupKeyOf(null)).toBeNull();
  });

  test("reads an issue's facts as the list route and a draw each hold them", () => {
    const route = issueFactsOf({
      key: "INFRA-1",
      status: "backlog",
      parent: null,
      labels: ["legion", 3],
      primary_artifact_id: "a",
    });
    expect(route).toEqual({
      key: "INFRA-1",
      facts: { parent: null, labels: ["legion"], status: "backlog", primaryArtifactId: "a" },
    });
    const held = issueFactsOf({
      key: "INFRA-1",
      status: "backlog",
      parent: "INFRA-0",
      labels: [],
      primaryArtifactId: "a",
    });
    expect(held?.facts.primaryArtifactId).toBe("a");
    expect(held?.facts.parent).toBe("INFRA-0");
    expect(issueFactsOf({ status: "backlog" })).toBeNull();
    expect(issueFactsOf("INFRA-1")).toBeNull();
  });

  test("reads the roots a draw recorded by key, and leaves out what is no issue", () => {
    const facts = rootFactsOf({
      issues: [draw().backlogLegionRoots.issues[0], { status: "backlog" }],
    });
    expect([...facts.keys()]).toEqual(["INFRA-1"]);
    expect(facts.get("INFRA-1")?.labels).toEqual(["legion"]);
    expect(rootFactsOf(undefined).size).toBe(0);
  });
});
