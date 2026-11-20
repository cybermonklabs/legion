import { afterEach, describe, expect, test } from "bun:test";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { CorpusError } from "../corpus-io";
import { renderingCheckOf } from "../draw";
import {
  type BacklogRoots,
  BUILD_CHAIN_NAMES,
  type BuildChain,
  chainRefusal,
  corpusDrawPath,
  corpusSlicesPath,
  type DrawRecord,
  type DrawRule,
  drawFactsOf,
  drawPath,
  type FinalTestTree,
  readDraw,
  readSlices,
  rootFactsOf,
  rootReadingOf,
  slicesOf,
  slicesPath,
} from "../draw-io";

/**
 * The owner of the two files one draw writes, read as the four commands that read them do.
 *
 * What it holds the module to is the one policy it chose for both: a directory with no draw
 * carries none, and a file that is there and is not what a draw writes stops the reading
 * rather than being read as a corpus that was never drawn. Which of the two a `draw.json` is,
 * is settled fact by fact where it is read, and on the type of each fact, as `readDraw`'s own
 * doc states it: a field that is there and is not of the type the record holds it for is the
 * file being no draw, while a field of that type saying nothing - a draw made before the field
 * was recorded, a cut that is no time, a group key written empty - is a draw recording none.
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

/**
 * What the fixture records of its renderings, written by the draw's own writer over a stand-in
 * for the sentence one rendering says of itself, so no wording the code no longer writes
 * stands here.
 */
const RENDERING_CHECK = renderingCheckOf(
  ["The text was handed to the rule as it was rendered."],
  "none"
);

/** The roots a draw that applied the never-decide rule recorded. */
const ROOTS: BacklogRoots = {
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
};

/** What that draw recorded of the rule. */
const RULE: DrawRule = {
  model: "anthropic/claude-opus-5-5",
  definitionSha256: "abc",
  cache: null,
  asked: 1,
  cached: 0,
  inputTokens: 10,
  outputTokens: 5,
  renderingCheck: RENDERING_CHECK,
};

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
    backlogLegionRoots: ROOTS,
    rule: RULE,
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

/** One draw written to a scratch file, which is the only way a reader is given one. */
async function written(record: unknown): Promise<string> {
  const path = drawPath(await scratch());
  await writeFile(path, `${JSON.stringify(record)}\n`, "utf8");
  return path;
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

  test("reads the facts of the record the draw wrote, under the corpus's own slices dir", async () => {
    const corpus = await scratch();
    await writeFile(drawPath(corpus), `${JSON.stringify(draw())}\n`, "utf8");
    expect(corpusDrawPath(corpus)).toBe(join(corpus, "slices", "draw.json"));
    expect(await readDraw(drawPath(corpus))).toEqual({
      cut: "2026-10-01T00:00:00.000Z",
      groupKey: "issue",
      roots: ROOTS.issues,
    });
  });

  test("carries no fact the draw does not record, and none that is no such fact", async () => {
    const { groupKey, ...before } = draw();
    expect(groupKey).toBe("issue");
    expect(await readDraw(await written(before))).not.toHaveProperty("groupKey");
    expect(await readDraw(await written(draw({ groupKey: "" })))).not.toHaveProperty("groupKey");
    expect(await readDraw(await written(draw({ cut: "the first of October" })))).not.toHaveProperty(
      "cut"
    );
    const { backlogLegionRoots, ...noRoots } = draw();
    expect(await readDraw(await written(noRoots))).not.toHaveProperty("roots");
  });

  test("reads a draw of the shape before the rule recorded what was checked", async () => {
    const now = draw();
    const { renderingCheck, ...older } = RULE;
    expect(renderingCheck).toBe(RENDERING_CHECK);
    expect(await readDraw(await written({ ...now, rule: older }))).toEqual({
      cut: "2026-10-01T00:00:00.000Z",
      groupKey: "issue",
      roots: ROOTS.issues,
    });
  });

  test("stops the reading where a fact is there and is not what the record holds it for", async () => {
    const cut = await written({ ...draw(), cut: 7 });
    await expect(readDraw(cut)).rejects.toThrow(`${cut} holds a cut that is no string`);
    const key = await written({ ...draw(), groupKey: ["issue"] });
    await expect(readDraw(key)).rejects.toThrow(`${key} holds a groupKey that is no string`);
    const roots = await written({ ...draw(), backlogLegionRoots: "INFRA-1" });
    await expect(readDraw(roots)).rejects.toThrow(
      `${roots} holds backlogLegionRoots that are no record`
    );
    const listed = await written({
      ...draw(),
      backlogLegionRoots: { readAt: "now", source: "dispatch", issues: 3 },
    });
    await expect(readDraw(listed)).rejects.toThrow(
      `${listed} holds no list of roots in backlogLegionRoots`
    );
  });

  test("stops the reading where a root is no issue, or holds a fact that is no such fact", async () => {
    const root = (overrides: Record<string, unknown>): Record<string, unknown> => ({
      ...(ROOTS.issues[0] as unknown as Record<string, unknown>),
      ...overrides,
    });
    const rootsOf = (...issues: unknown[]): DrawRecord => ({
      ...draw(),
      backlogLegionRoots: { readAt: "now", source: "dispatch", issues } as never,
    });
    const noIssue = await written(rootsOf("INFRA-1"));
    await expect(readDraw(noIssue)).rejects.toThrow(`${noIssue} root 1 is no issue`);
    const noStatus = await written(rootsOf(root({ status: undefined })));
    await expect(readDraw(noStatus)).rejects.toThrow(
      `${noStatus} root INFRA-1 names no key or no status`
    );
    // With no key to name the root by, the refusal says which entry of the list it was.
    const noKey = await written(rootsOf(root({ key: undefined })));
    await expect(readDraw(noKey)).rejects.toThrow(`${noKey} root 1 names no key or no status`);
    const labels = await written(rootsOf(root({ labels: "legion" })));
    await expect(readDraw(labels)).rejects.toThrow(
      `${labels} root INFRA-1 holds labels that are no list`
    );
    const label = await written(rootsOf(root({ labels: ["legion", 3] })));
    await expect(readDraw(label)).rejects.toThrow(
      `${label} root INFRA-1 names a label that is no string: 3`
    );
    const parent = await written(rootsOf(root({ parent: 3 })));
    await expect(readDraw(parent)).rejects.toThrow(
      `${parent} root INFRA-1 holds a parent that is no string`
    );
  });

  test("reads a root that records no parent and no document as naming neither", async () => {
    const path = await written({
      ...draw(),
      backlogLegionRoots: {
        readAt: "now",
        source: "dispatch",
        issues: [{ key: "INFRA-1", status: "backlog" }],
      },
    });
    expect((await readDraw(path))?.roots).toEqual([
      { key: "INFRA-1", parent: null, labels: [], status: "backlog", primaryArtifactId: null },
    ]);
  });

  test("reads the final test's trees, and none from a draw that recorded none", async () => {
    const trees: FinalTestTree[] = [
      { group: "tree:RES-0", slice: "report", cases: ["r-0-0", "r-0-1"] },
      { group: "tree:RES-1", slice: null, cases: ["r-1-0"] },
    ];
    const path = await written({ ...draw(), finalTest: { reference: "claim:test", trees } });
    expect((await readDraw(path))?.finalTestTrees).toEqual(trees);
    // A final test recorded before its trees were, and a draw that read no reserve, carry none.
    const before = await written({ ...draw(), finalTest: { reference: "claim:test" } });
    expect(await readDraw(before)).not.toHaveProperty("finalTestTrees");
    expect(await readDraw(await written({ ...draw(), finalTest: null }))).not.toHaveProperty(
      "finalTestTrees"
    );
  });

  test("stops the reading where a final-test tree cannot be counted", async () => {
    const withTrees = (trees: unknown): Promise<string> =>
      written({ ...draw(), finalTest: { reference: "claim:test", trees } });
    const record = await written({ ...draw(), finalTest: "claim:test" });
    await expect(readDraw(record)).rejects.toThrow(`${record} holds a finalTest that is no record`);
    const list = await withTrees("tree:RES-0");
    await expect(readDraw(list)).rejects.toThrow(`${list} holds finalTest trees that are no list`);
    const group = await withTrees([{ slice: "report", cases: [] }]);
    await expect(readDraw(group)).rejects.toThrow(`${group} finalTest tree 1 names no group`);
    // A tree in no slice is written with a slice of null; one with no slice at all is no tree,
    // and a claim draw puts every eligible tree in report, so one in fit is no such draw.
    for (const slice of [undefined, "test", "fit"]) {
      const path = await withTrees([{ group: "tree:RES-0", slice, cases: ["r-0-0"] }]);
      await expect(readDraw(path)).rejects.toThrow(
        `${path} finalTest tree tree:RES-0 names no slice, or another than report`
      );
    }
    const cases = await withTrees([{ group: "tree:RES-0", slice: "report", cases: "r-0-0" }]);
    await expect(readDraw(cases)).rejects.toThrow(
      `${cases} finalTest tree tree:RES-0 holds no list of cases`
    );
    const twice = await withTrees([
      { group: "tree:RES-0", slice: "report", cases: ["r-0-0"] },
      { group: "tree:RES-1", slice: "report", cases: ["r-0-0"] },
    ]);
    await expect(readDraw(twice)).rejects.toThrow(
      `${twice} names the case r-0-0 in the trees tree:RES-0 and tree:RES-1`
    );
    // One tree written as two, with no case in common, would count twice.
    const split = await withTrees([
      { group: "tree:RES-0", slice: "report", cases: ["r-0-0"] },
      { group: "tree:RES-0", slice: "report", cases: ["r-0-1"] },
    ]);
    await expect(readDraw(split)).rejects.toThrow(
      `${split} names the finalTest tree tree:RES-0 twice`
    );
  });

  test("reads a claim draw's run, its read's reference and that it applied no rule", async () => {
    const claimRun: BuildChain = {
      id: "start-1",
      snapshotSha256: "a".repeat(64),
      corpusSha256: "b".repeat(64),
      treesSha256: "c".repeat(64),
    };
    const claim = {
      ...draw({ backlogLegionRootsRead: null, backlogLegionRoots: null, rule: null }),
      neverDecideApplied: false,
      claimRun,
      finalTest: { reference: "claim:test", trees: [] },
    };
    const text = `${JSON.stringify(claim)}\n`;
    const path = await written(claim);
    const facts = await readDraw(path);
    expect(facts).toEqual({
      cut: "2026-10-01T00:00:00.000Z",
      groupKey: "issue",
      finalTestTrees: [],
      finalTestReference: "claim:test",
      neverDecideApplied: false,
      claimRun,
    });
    // The chain read back is the one a later stage of the run holds its own to.
    expect(chainRefusal(facts?.claimRun, claimRun, BUILD_CHAIN_NAMES)).toBeNull();
    // The text a caller read itself gives the same facts, so its digest is of those bytes.
    expect(drawFactsOf(text, path)).toEqual(facts ?? {});
    // A draw that read no reserve records no claim run, and one made before either was recorded
    // carries neither.
    const plain = await readDraw(await written(draw({ claimRun: null })));
    expect(plain).not.toHaveProperty("claimRun");
    expect(plain).not.toHaveProperty("neverDecideApplied");
  });

  test("stops the reading where a claim draw's run or rule record is no such fact", async () => {
    const whole = {
      id: "start-1",
      snapshotSha256: "a".repeat(64),
      corpusSha256: "b".repeat(64),
      treesSha256: "c".repeat(64),
    };
    const { treesSha256, ...noTrees } = whole;
    expect(treesSha256).toHaveLength(64);
    // No reply id, or a sha256 of one of the three files missing or no 64 lowercase hex digits.
    const malformed: unknown[] = [
      { id: "start-1" },
      { ...whole, id: "" },
      { ...whole, id: 7 },
      noTrees,
      { ...whole, snapshotSha256: "A".repeat(64) },
      { ...whole, corpusSha256: "b".repeat(63) },
      { ...whole, treesSha256: 7 },
      "x",
    ];
    for (const claimRun of malformed) {
      const path = await written({ ...draw(), claimRun });
      await expect(readDraw(path)).rejects.toThrow(
        `${path} holds a claimRun that names no reply id, or no sha256 of each of the build's`
      );
    }
    const applied = await written({ ...draw(), neverDecideApplied: "no" });
    await expect(readDraw(applied)).rejects.toThrow(
      `${applied} holds a neverDecideApplied that is no boolean`
    );
    const reference = await written({ ...draw(), finalTest: { reference: 7, trees: [] } });
    await expect(readDraw(reference)).rejects.toThrow(
      `${reference} finalTest holds a reference that is no string`
    );
  });

  test("stops the reading where neverDecideApplied disagrees with its rule and roots", async () => {
    // One fact in three fields: what the draw says it applied, its rule, and its roots.
    const disagreeing: Array<[boolean, DrawRule | null, BacklogRoots | null, string]> = [
      [false, RULE, ROOTS, "of false beside a rule and backlog roots"],
      [false, RULE, null, "of false beside a rule and no backlog roots"],
      [false, null, ROOTS, "of false beside no rule and backlog roots"],
      [true, null, ROOTS, "of true beside no rule and backlog roots"],
      [true, RULE, null, "of true beside a rule and no backlog roots"],
      [true, null, null, "of true beside no rule and no backlog roots"],
    ];
    for (const [neverDecideApplied, rule, backlogLegionRoots, words] of disagreeing) {
      const path = await written(draw({ neverDecideApplied, rule, backlogLegionRoots }));
      await expect(readDraw(path)).rejects.toThrow(CorpusError);
      await expect(readDraw(path)).rejects.toThrow(`${path} holds a neverDecideApplied ${words}`);
    }
    // A rule the record does not hold at all is no rule either.
    const absent = await written({ ...draw({ neverDecideApplied: true }), rule: undefined });
    await expect(readDraw(absent)).rejects.toThrow(
      `${absent} holds a neverDecideApplied of true beside no rule and backlog roots`
    );
  });

  test("reads a draw whose neverDecideApplied agrees with the rule and the roots", async () => {
    expect(await readDraw(await written(draw({ neverDecideApplied: true })))).toEqual({
      cut: "2026-10-01T00:00:00.000Z",
      groupKey: "issue",
      roots: ROOTS.issues,
      neverDecideApplied: true,
    });
    const unruled = draw({ neverDecideApplied: false, rule: null, backlogLegionRoots: null });
    expect(await readDraw(await written(unruled))).toEqual({
      cut: "2026-10-01T00:00:00.000Z",
      groupKey: "issue",
      neverDecideApplied: false,
    });
    // A draw made before the field was recorded says nothing for its rule and roots to agree with.
    const before = await readDraw(await written(draw({ rule: null })));
    expect(before).not.toHaveProperty("neverDecideApplied");
    expect(before?.roots).toEqual(ROOTS.issues);
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

  test("reads the text a caller read itself as the file is read", async () => {
    const text = JSON.stringify({ report: ["case-b"] });
    expect(slicesOf(text, "/draw/slices.json")).toEqual({
      fit: [],
      report: ["case-b"],
      anchor: [],
      reserve: [],
    });
    expect(() => slicesOf("[]", "/draw/slices.json")).toThrow(
      "/draw/slices.json is not a draw's slices"
    );
  });
});

/**
 * The one reader of a root, which the three places that read one all go through: a draw's own
 * `backlogLegionRoots`, the issue list route, and the file `--backlog-roots` names. Its policy
 * is refusal and never a value dropped, because a root dropped is a root the never-decide rule
 * is never told about: the issue list's loop used to pass over an entry whose key or status it
 * could not read, and a parked `legion` root passed over that way is a hold that silently does
 * not happen. Every field is held to that policy, the labels among them.
 */
describe("the facts a reader takes out of a draw", () => {
  test("reads a root's fields as the list route and a draw each hold them", () => {
    const route = rootReadingOf({
      key: "INFRA-1",
      status: "backlog",
      parent: null,
      labels: ["legion"],
      primary_artifact_id: "a",
    });
    expect(route).toEqual({
      root: {
        key: "INFRA-1",
        parent: null,
        labels: ["legion"],
        status: "backlog",
        primaryArtifactId: "a",
      },
      key: "INFRA-1",
      fault: null,
    });
    const held = rootReadingOf({
      key: "INFRA-1",
      status: "backlog",
      parent: "INFRA-0",
      labels: [],
      primaryArtifactId: "a",
    });
    expect(held.root?.primaryArtifactId).toBe("a");
    expect(held.root?.parent).toBe("INFRA-0");
  });

  test("refuses a root holding a field that is no such field, and quotes the label", () => {
    expect(rootReadingOf("INFRA-1")).toEqual({ root: null, key: null, fault: "is no issue" });
    expect(rootReadingOf({ status: "backlog" })).toEqual({
      root: null,
      key: null,
      fault: "names no key or no status",
    });
    expect(rootReadingOf({ key: 7, status: "backlog" })).toEqual({
      root: null,
      // The key is itself the bad field, so the caller says which entry of the list it was.
      key: null,
      fault: "holds a key that is no string",
    });
    expect(rootReadingOf({ key: "INFRA-1", status: 7 })).toEqual({
      root: null,
      // The entry names a key, so a refusal over it names the root and not its place.
      key: "INFRA-1",
      fault: "holds a status that is no string",
    });
    expect(rootReadingOf({ key: "INFRA-1", status: "backlog", labels: "legion" })).toEqual({
      root: null,
      key: "INFRA-1",
      fault: "holds labels that are no list",
    });
    const label = rootReadingOf({ key: "INFRA-1", status: "backlog", labels: ["legion", 7] });
    expect(label).toEqual({
      root: null,
      key: "INFRA-1",
      fault: "names a label that is no string: 7",
    });
    expect(rootReadingOf({ key: "INFRA-1", status: "backlog", primary_artifact_id: 7 })).toEqual({
      root: null,
      key: "INFRA-1",
      fault: "holds a primary_artifact_id that is no string",
    });
  });

  test("reads the roots a draw recorded by key", () => {
    const facts = rootFactsOf(ROOTS.issues);
    expect([...facts.keys()]).toEqual(["INFRA-1"]);
    expect(facts.get("INFRA-1")?.labels).toEqual(["legion"]);
    expect(rootFactsOf(undefined).size).toBe(0);
    expect(rootFactsOf([]).size).toBe(0);
  });
});

/**
 * The one comparison every stage of a claim run holds a record's chain to: the first field the
 * names list in which the record read back is not this run's, worded as the tail of a refusal,
 * and nothing where every such field is the same.
 */
describe("chainRefusal", () => {
  const chain: BuildChain = {
    id: "start-1",
    snapshotSha256: "a".repeat(64),
    corpusSha256: "b".repeat(64),
    treesSha256: "c".repeat(64),
  };
  const other = "d".repeat(64);

  test("finds nothing to refuse in a chain equal to this run's", () => {
    expect(chainRefusal({ ...chain }, chain, BUILD_CHAIN_NAMES)).toBeNull();
  });

  test("names the first field that differs, in the order the chain's names list them", () => {
    const names = Object.values(BUILD_CHAIN_NAMES);
    expect(names).toEqual(["claim run", "snapshot.json", "corpus.jsonl", "trees.json"]);
    const all = { id: "start-2", snapshotSha256: other, corpusSha256: other, treesSha256: other };
    expect(chainRefusal(all, chain, BUILD_CHAIN_NAMES)).toBe(
      "its claim run is start-2, and this run's is start-1"
    );
    expect(chainRefusal({ ...all, id: "start-1" }, chain, BUILD_CHAIN_NAMES)).toBe(
      `its snapshot.json is ${other}, and this run's is ${chain.snapshotSha256}`
    );
    const corpus = { ...chain, corpusSha256: other, treesSha256: other };
    expect(chainRefusal(corpus, chain, BUILD_CHAIN_NAMES)).toBe(
      `its corpus.jsonl is ${other}, and this run's is ${chain.corpusSha256}`
    );
    expect(chainRefusal({ ...chain, treesSha256: other }, chain, BUILD_CHAIN_NAMES)).toBe(
      `its trees.json is ${other}, and this run's is ${chain.treesSha256}`
    );
  });

  test("reads a field the record does not hold, or holds as null, as of no record", () => {
    expect(chainRefusal({ id: "start-1" }, chain, BUILD_CHAIN_NAMES)).toBe(
      `its snapshot.json is of no record, and this run's is ${chain.snapshotSha256}`
    );
    expect(chainRefusal({ ...chain, treesSha256: null }, chain, BUILD_CHAIN_NAMES)).toBe(
      `its trees.json is of no record, and this run's is ${chain.treesSha256}`
    );
    // What is no record at all holds no field, so its first field is of no record.
    for (const found of [undefined, null, "start-1", [chain]]) {
      expect(chainRefusal(found, chain, BUILD_CHAIN_NAMES)).toBe(
        "its claim run is of no record, and this run's is start-1"
      );
    }
  });

  test("compares only the fields the names list", () => {
    const files = { ...chain, snapshotSha256: other, corpusSha256: other, treesSha256: other };
    expect(chainRefusal(files, chain, { id: "claim run" })).toBeNull();
    expect(chainRefusal({ ...chain, read: "claim:other" }, chain, BUILD_CHAIN_NAMES)).toBeNull();
  });
});
