import { afterEach, describe, expect, spyOn, test } from "bun:test";
import { mkdir, mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { type CaseRow, countsOf } from "../build-corpus";
import { type Corpus, CorpusError, readCorpus } from "../corpus-io";
import type { DispatchEvent } from "../dispatch-read";
import { FreshError, type FreshReport, refuseReportsOutside } from "../fresh-agent";
import { PoolError, type RunOutcomes, refuseRunsOutside } from "../pool-runs";
import {
  DigestError,
  type IssueParents,
  noteAskRoot,
  noteIssueParent,
  pickedRoot,
  RESERVE_TREE_CUT,
  RESERVE_VARIABLE,
  type Registration,
  readReferenceOf,
  reserveOf,
  runDigestOf,
  treeRootOf,
} from "../reserve";

/**
 * The reserve of INFRA-2897's next final test, against the rule as it was registered
 * (dispatch://INFRA/artifact/infra-2897-final-test-reserve-md), and `readCorpus` holding its cases
 * out of what every command reads. Nothing here reads Dispatch or a real corpus.
 */

/** A root key the registered rule picks (its digit is 2) and one it does not (its digit is c). */
const PICKED = "INFRA-3189";
const NOT_PICKED = "INFRA-2523";

/** A time after the cut. */
const LATER = "2026-11-16T01:00:00Z";

/**
 * One case, with only the fields the reserve reads given values that matter: its ask opened when
 * it was answered, and its issue is the root of its tree at the opening and at the build.
 */
function caseRow(caseId: string, issueKey: string | null, answeredAt: string): CaseRow {
  return {
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
    askTreeRoot: issueKey,
    issueTreeRoot: issueKey,
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
}

/** A case on INFRA-4001 whose ask opened at `openedAt` under `root`, and under `buildRoot` later. */
function treeCase(
  caseId: string,
  openedAt: string,
  root: string | null,
  buildRoot: string | null = root
): CaseRow {
  const answeredAt = new Date(Date.parse(openedAt) + 3_600_000).toISOString();
  return {
    ...caseRow(caseId, "INFRA-4001", answeredAt),
    askCreatedAt: openedAt,
    askTreeRoot: root,
    issueTreeRoot: buildRoot,
  };
}

/** The same case as a corpus built before `build-corpus.ts` recorded its tree's roots. */
function olderBuild(row: CaseRow): CaseRow {
  const older = { ...row };
  delete older.askTreeRoot;
  delete older.issueTreeRoot;
  return older;
}

describe("the registered rule", () => {
  test("picks the registration's worked examples as it says", () => {
    expect(pickedRoot(PICKED)).toBe(true);
    expect(pickedRoot(NOT_PICKED)).toBe(false);
  });

  test("holds an ask opened at the cut under a picked root, and keeps one opened just before", () => {
    expect(reserveOf(treeCase("at-cut", RESERVE_TREE_CUT, PICKED))).toBe("held");
    const before = new Date(Date.parse(RESERVE_TREE_CUT) - 1).toISOString();
    expect(reserveOf(treeCase("before-cut", before, PICKED))).toBe("kept");
  });

  test("keeps an ask under a root it does not pick, one with no root, and one on no issue", () => {
    expect(reserveOf(treeCase("not-picked", LATER, NOT_PICKED))).toBe("kept");
    expect(reserveOf(treeCase("no-root", LATER, null))).toBe("kept");
    expect(reserveOf(caseRow("no-issue", null, LATER))).toBe("kept");
  });

  test("holds an ask whose issue moved, when the rule picks its root at the opening or the build", () => {
    expect(reserveOf(treeCase("moved-out", LATER, PICKED, NOT_PICKED))).toBe("held");
    expect(reserveOf(treeCase("moved-in", LATER, NOT_PICKED, PICKED))).toBe("held");
    expect(reserveOf(treeCase("lost", LATER, null, PICKED))).toBe("held");
  });

  test("places an ask with no opening time by its answer", () => {
    expect(reserveOf({ ...treeCase("late", LATER, PICKED), askCreatedAt: null })).toBe("held");
    const early = { ...treeCase("early", "2026-11-15T01:00:00Z", PICKED), askCreatedAt: null };
    expect(reserveOf(early)).toBe("kept");
  });

  test("cannot say for a corpus built before trees, unless the ask opened before the cut", () => {
    expect(reserveOf(olderBuild(treeCase("old-build", LATER, PICKED)))).toBe("unknown");
    const justBefore = new Date(Date.parse(RESERVE_TREE_CUT) - 60_000).toISOString();
    const oldAsk = olderBuild(treeCase("old-ask", justBefore, PICKED));
    expect(Date.parse(oldAsk.answeredAt)).toBeGreaterThan(Date.parse(RESERVE_TREE_CUT));
    expect(reserveOf(oldAsk)).toBe("kept");
  });

  test("keeps what the released version 1 held: a picked issue key in a tree the rule does not pick", () => {
    // Version 1 held INFRA-3601, created after 2026-11-14T06:00:00Z; the owner released it.
    const opened = "2026-11-16T02:00:00Z";
    const row = {
      ...caseRow("version-1", "INFRA-3601", opened),
      askTreeRoot: NOT_PICKED,
      issueTreeRoot: NOT_PICKED,
    };
    expect(reserveOf(row)).toBe("kept");
    const before = caseRow("version-1-before", "INFRA-3601", "2026-11-15T01:00:00Z");
    expect(reserveOf(before)).toBe("kept");
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

describe("an ask's tree, from the walk of the log", () => {
  function event(id: number, type: string, payload: Record<string, unknown>): DispatchEvent {
    return {
      id,
      issue_key: null,
      project: "INFRA",
      seq: id,
      type,
      actor: null,
      created_at: `2026-11-16T00:00:0${id}Z`,
      payload,
    };
  }

  function walk(events: DispatchEvent[]): {
    parents: IssueParents;
    roots: Map<string, string | null>;
  } {
    const parents: IssueParents = new Map();
    const roots = new Map<string, string | null>();
    for (const one of events) {
      noteIssueParent(parents, one);
      noteAskRoot(roots, parents, one);
    }
    return { parents, roots };
  }

  test("keeps the root an ask had at its first opening, whatever moves after", () => {
    const { parents, roots } = walk([
      event(1, "issue.created", { key: "INFRA-1", parent: null }),
      event(2, "issue.created", { key: "INFRA-2", parent: "INFRA-1" }),
      event(3, "ask.opened", { id: "first", issue_key: "INFRA-2" }),
      event(4, "issue.created", { key: "INFRA-3", parent: null }),
      event(5, "issue.updated", { key: "INFRA-2", parent: "INFRA-3" }),
      event(6, "ask.opened", { id: "second", issue_key: "INFRA-2" }),
      event(7, "ask.opened", { id: "first", issue_key: "INFRA-3" }),
    ]);
    expect([...roots]).toEqual([
      ["first", "INFRA-1"],
      ["second", "INFRA-3"],
    ]);
    expect(treeRootOf("INFRA-2", parents)).toBe("INFRA-3");
  });

  test("names no root on no issue, an issue the walk has not read, or a cycle", () => {
    const parents: IssueParents = new Map([
      ["INFRA-5", "INFRA-6"],
      ["INFRA-6", "INFRA-5"],
      ["INFRA-7", "INFRA-8"],
    ]);
    expect(treeRootOf("INFRA-4", parents)).toBeNull();
    expect(treeRootOf("INFRA-5", parents)).toBeNull();
    expect(treeRootOf("INFRA-7", parents)).toBeNull();
    const { roots } = walk([event(1, "ask.opened", { id: "no-issue" })]);
    expect(roots.get("no-issue")).toBeNull();
  });

  test("reads a parent only from an issue event that carries the field", () => {
    const { parents } = walk([
      event(1, "issue.created", { key: "INFRA-1", parent: "INFRA-9" }),
      event(2, "issue.released", { key: "INFRA-1" }),
      event(3, "comment.created", { key: "INFRA-1", parent: null }),
      event(4, "issue.updated", { key: "INFRA-2", parent: "" }),
    ]);
    expect(parents.get("INFRA-1")).toBe("INFRA-9");
    expect(parents.get("INFRA-2")).toBeNull();
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

/** A repository root holding a bundle of two files, beside the registration and a test. */
async function bundleRoot(): Promise<string> {
  const root = await mkdtemp(join(tmpdir(), "bundle-"));
  made.push(root);
  await mkdir(join(root, "scripts/approval-eval/__tests__"), { recursive: true });
  await mkdir(join(root, "skills/approval-review/references"), { recursive: true });
  await writeFile(join(root, "scripts/approval-eval/run-round.ts"), "the arms\n", "utf8");
  await writeFile(join(root, "scripts/approval-eval/claim.ts"), "no read\n", "utf8");
  await writeFile(join(root, "scripts/approval-eval/__tests__/a.test.ts"), "a test\n", "utf8");
  await writeFile(join(root, "skills/approval-review/references/rule.md"), "a rule\n", "utf8");
  return root;
}

/** `readCorpus` under a registration and a bundle root, with what it printed to stderr. */
async function read(
  directory: string,
  registration: Registration = { reads: [], ended: null },
  root?: string
): Promise<{ corpus: Corpus; printed: string }> {
  const lines: string[] = [];
  const error = spyOn(console, "error").mockImplementation((line?: unknown) => {
    lines.push(String(line));
  });
  try {
    const corpus = await readCorpus(directory, { registration, root });
    return { corpus, printed: lines.join("\n") };
  } finally {
    error.mockRestore();
  }
}

describe("runDigestOf", () => {
  const command = ["/usr/local/bin/bun", "/repository/scripts/approval-eval/run-round.ts"];
  const env = { ANTHROPIC_BASE_URL: "http://gateway:4000", ANTHROPIC_API_KEY: "key-one" };

  test("is the same for the same run, and moves with a file of the bundle or the command", async () => {
    const root = await bundleRoot();
    const first = await runDigestOf({ argv: command, env }, root);
    expect(first).toMatch(/^[0-9a-f]{64}$/);
    expect(await runDigestOf({ argv: command, env }, root)).toBe(first);
    const draw = ["/usr/local/bin/bun", "/repository/scripts/approval-eval/draw.ts"];
    expect(await runDigestOf({ argv: draw, env }, root)).not.toBe(first);
    await writeFile(join(root, "skills/approval-review/references/rule.md"), "a rule.\n", "utf8");
    const skillMoved = await runDigestOf({ argv: command, env }, root);
    expect(skillMoved).not.toBe(first);
    await writeFile(join(root, "scripts/approval-eval/run-round.ts"), "other arms\n", "utf8");
    expect(await runDigestOf({ argv: command, env }, root)).not.toBe(skillMoved);
  });

  test("leaves out the registration and the tests, so registering a run keeps its digest", async () => {
    const root = await bundleRoot();
    const before = await runDigestOf({ argv: command, env }, root);
    await writeFile(join(root, "scripts/approval-eval/claim.ts"), "one read\n", "utf8");
    await writeFile(join(root, "scripts/approval-eval/__tests__/a.test.ts"), "changed\n", "utf8");
    expect(await runDigestOf({ argv: command, env }, root)).toBe(before);
  });

  test("refuses a link in the bundle, since a change behind it would not move the digest", async () => {
    const root = await bundleRoot();
    await symlink(
      join(root, "skills/approval-review/references/rule.md"),
      join(root, "scripts/approval-eval/linked.md")
    );
    await expect(runDigestOf({ argv: command, env }, root)).rejects.toThrow(DigestError);
  });

  test("takes a file option by its bytes and a directory option as a path", async () => {
    const root = await bundleRoot();
    const one = await mkdtemp(join(tmpdir(), "arms-"));
    const two = await mkdtemp(join(tmpdir(), "arms-"));
    made.push(one, two);
    const arms = join(one, "arms.json");
    await writeFile(arms, "[1]\n", "utf8");
    const run = (...args: string[]) => runDigestOf({ argv: [...command, ...args], env }, root);
    const pinned = await run("--arms", arms, "--corpus", one, "--out", "/nowhere/a");
    expect(await run(`--arms=${arms}`, "--corpus", two, "--out", "/nowhere/b")).toBe(pinned);
    await writeFile(arms, "[2]\n", "utf8");
    expect(await run("--arms", arms, "--corpus", one, "--out", "/nowhere/a")).not.toBe(pinned);
  });

  test("reads a file option at its trimmed path, as the commands do", async () => {
    const root = await bundleRoot();
    const directory = await mkdtemp(join(tmpdir(), "distillation-"));
    made.push(directory);
    const file = join(directory, "distillation.md");
    await writeFile(file, "first\n", "utf8");
    const run = () => runDigestOf({ argv: [...command, "--distillation", `${file} `], env }, root);
    const before = await run();
    await writeFile(file, "second\n", "utf8");
    expect(await run()).not.toBe(before);
  });

  test("takes the argument after a file option as its value, as parseArgs does", async () => {
    const root = await bundleRoot();
    const run = (...args: string[]) => runDigestOf({ argv: [...command, ...args], env }, root);
    expect(await run("--arms", "-")).toBe(await run("--arms=-"));
  });

  test("refuses a file option that names something other than a regular file", async () => {
    const root = await bundleRoot();
    const directory = await mkdtemp(join(tmpdir(), "arms-"));
    made.push(directory);
    await expect(
      runDigestOf({ argv: [...command, "--arms", directory], env }, root)
    ).rejects.toThrow(DigestError);
  });

  test("takes any other value as written, whatever the disk holds under that name", async () => {
    const root = await bundleRoot();
    const run = (arm: string) => runDigestOf({ argv: [...command, "--only", arm], env }, root);
    expect(await run(join(root, "scripts/approval-eval"))).not.toBe(
      await run(join(root, "skills/approval-review"))
    );
  });

  test("reads an option as a file only in a command where it names one", async () => {
    const root = await bundleRoot();
    const directory = await mkdtemp(join(tmpdir(), "cases-"));
    made.push(directory);
    const of = (name: string) => [
      `/repository/scripts/approval-eval/${name}`,
      "--cases",
      directory,
    ];
    const fresh = ["/usr/local/bin/bun", ...of("fresh-agent.ts")];
    expect(await runDigestOf({ argv: fresh, env }, root)).toMatch(/^[0-9a-f]{64}$/);
    const analyze = ["/usr/local/bin/bun", ...of("analyze.ts")];
    await expect(runDigestOf({ argv: analyze, env }, root)).rejects.toThrow(DigestError);
  });

  test("refuses an agent PATH does not find, since the runtime could find another", async () => {
    const root = await bundleRoot();
    const argv = ["/usr/local/bin/bun", "/repository/scripts/approval-eval/fresh-agent.ts"];
    const unfound = { ...env, PATH: "" };
    await expect(
      runDigestOf({ argv: [...argv, "--omp", "printf"], env: unfound }, root)
    ).rejects.toThrow(DigestError);
  });

  test("moves with the servers a run reads and the lines a command adds, not with a key", async () => {
    const root = await bundleRoot();
    const first = await runDigestOf({ argv: command, env }, root);
    const otherKey = { ...env, ANTHROPIC_API_KEY: "key-two" };
    expect(await runDigestOf({ argv: command, env: otherKey }, root)).toBe(first);
    const otherServer = { ...env, ANTHROPIC_BASE_URL: "http://elsewhere:4000" };
    expect(await runDigestOf({ argv: command, env: otherServer }, root)).not.toBe(first);
    const otherDispatch = { ...env, DISPATCH_URL: "http://dispatch:8766" };
    expect(await runDigestOf({ argv: command, env: otherDispatch }, root)).not.toBe(first);
    const agent = async () => ["agent models 1234"];
    expect(await runDigestOf({ argv: command, env, inputs: agent }, root)).not.toBe(first);
  });
});

describe("readCorpus and the reserve", () => {
  const reserved = caseRow("reserved", PICKED, LATER);
  const sameIssueQuestion: CaseRow = { ...caseRow("question", PICKED, LATER), kind: "question" };
  const child: CaseRow = {
    ...caseRow("child", "INFRA-4001", LATER),
    askTreeRoot: PICKED,
    issueTreeRoot: PICKED,
  };
  const open = caseRow("open", NOT_PICKED, LATER);
  const claim = "dispatch://INFRA-2897/comment/claim-run";

  test("holds every case of a reserved tree out of the rows and the ids, and says so", async () => {
    delete process.env[RESERVE_VARIABLE];
    const directory = await corpusOnDisk([reserved, sameIssueQuestion, child, open]);
    const { corpus, printed } = await read(directory);
    expect(corpus.rows.map((row) => row.caseId)).toEqual(["open"]);
    for (const caseId of ["reserved", "question", "child"]) {
      expect(corpus.byCaseId.has(caseId)).toBe(false);
    }
    expect(corpus.reserve).toEqual({ cases: 3, issues: 2, readUnder: null });
    expect(printed).toContain("held out 3 cases on 2 issues");
  });

  test("reads them under a reference registered for this run, and records it", async () => {
    process.env[RESERVE_VARIABLE] = `read:${claim}`;
    const root = await bundleRoot();
    const run = await runDigestOf({ argv: process.argv, env: process.env }, root);
    const registration = { reads: [{ reference: claim, run }], ended: null };
    const directory = await corpusOnDisk([reserved, open]);
    const { corpus, printed } = await read(directory, registration, root);
    expect(corpus.rows.map((row) => row.caseId)).toEqual(["reserved", "open"]);
    expect(corpus.reserve.readUnder).toBe(claim);
    expect(printed).toContain(`under ${claim}`);
  });

  test("refuses a reference no read registers, and names this run's digest", async () => {
    process.env[RESERVE_VARIABLE] = `read:${claim}`;
    const root = await bundleRoot();
    const run = await runDigestOf({ argv: process.argv, env: process.env }, root);
    const refused = readCorpus(await corpusOnDisk([reserved, open]), {
      registration: { reads: [], ended: null },
      root,
    });
    await expect(refused).rejects.toThrow(CorpusError);
    await expect(refused).rejects.toThrow(`registers no read under ${claim}`);
    await expect(refused).rejects.toThrow(run);
  });

  test("refuses a registered reference for a run with another digest", async () => {
    process.env[RESERVE_VARIABLE] = `read:${claim}`;
    const root = await bundleRoot();
    const other: Registration = { reads: [{ reference: claim, run: "0".repeat(64) }], ended: null };
    await expect(
      readCorpus(await corpusOnDisk([reserved, open]), { registration: other, root })
    ).rejects.toThrow(/for other runs/);
  });

  test("refuses a value of the variable that names no reference", async () => {
    process.env[RESERVE_VARIABLE] = "read";
    await expect(readCorpus(await corpusOnDisk([open]))).rejects.toThrow(CorpusError);
  });

  test("holds nothing once the reserve ended, an older build included", async () => {
    delete process.env[RESERVE_VARIABLE];
    const oldBuild = olderBuild(caseRow("old-build", PICKED, LATER));
    const ended: Registration = { reads: [], ended: "dispatch://INFRA-2897/comment/claim-result" };
    const { corpus, printed } = await read(await corpusOnDisk([reserved, oldBuild, open]), ended);
    expect(corpus.rows.map((row) => row.caseId)).toEqual(["reserved", "old-build", "open"]);
    expect(corpus.reserve).toEqual({ cases: 0, issues: 0, readUnder: null });
    expect(printed).toBe("");
  });

  test("refuses a corpus built before trees that holds an ask opened after the cut", async () => {
    delete process.env[RESERVE_VARIABLE];
    const oldBuild = olderBuild(caseRow("old-build", NOT_PICKED, LATER));
    await expect(readCorpus(await corpusOnDisk([oldBuild, open]))).rejects.toThrow(
      /build it again/
    );
  });

  test("reads a corpus built before the cut as it always did", async () => {
    delete process.env[RESERVE_VARIABLE];
    const old = olderBuild(caseRow("old", PICKED, "2026-11-10T01:00:00Z"));
    const { corpus, printed } = await read(await corpusOnDisk([old]));
    expect(corpus.rows.map((row) => row.caseId)).toEqual(["old"]);
    expect(corpus.reserve.cases).toBe(0);
    expect(printed).toBe("");
  });
});

describe("what a command prints from a file an earlier run wrote", () => {
  test("a build's counts leave a reserved case's verdict out", () => {
    const reserved: CaseRow = { ...caseRow("reserved", PICKED, LATER), verdict: "request_changes" };
    const counts = countsOf([reserved, caseRow("open", NOT_PICKED, LATER)]);
    expect(counts.reserved).toBe(1);
    expect(counts.total).toBe(1);
    expect(counts.approvalByVerdict).toEqual({ approve: 1, request_changes: 0, other: 0 });
  });

  /** The ids a read of a corpus holding one reserved and one open case leaves readable. */
  async function readable(): Promise<Map<string, CaseRow>> {
    delete process.env[RESERVE_VARIABLE];
    const reserved = caseRow("reserved", PICKED, LATER);
    const open = caseRow("open", NOT_PICKED, LATER);
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
