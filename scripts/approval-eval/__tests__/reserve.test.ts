import { afterEach, describe, expect, spyOn, test } from "bun:test";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  type CaseRow,
  countsOf,
  type Snapshot,
  takeReserveList,
  writeCorpus,
} from "../build-corpus";
import {
  CannotRunError,
  type Corpus,
  CorpusError,
  readCorpus,
  type SnapshotReserve,
} from "../corpus-io";
import type { CommentReply, DispatchEvent } from "../dispatch-read";
import { FreshError, type FreshReport, refuseReportsOutside } from "../fresh-agent";
import { PoolError, type RunOutcomes, refuseRunsOutside } from "../pool-runs";
import {
  CLAIM_RUN_START,
  CLAIM_RUN_VARIABLE,
  claimRunOf,
  DigestError,
  type IssueParents,
  noteAskRoot,
  noteIssueParent,
  pickedRoot,
  RESERVE_LIST_CUT,
  RESERVE_TREE_CUT,
  RESERVE_VARIABLE,
  type Registration,
  type RepliesOf,
  readReferenceOf,
  registrationCommentOf,
  reserveOf,
  runDigestOf,
  treeRootOf,
} from "../reserve";
import { TreeLog } from "../trees";

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

  test("keeps every ask of a tree a read spent, by its root at the opening or at the build", () => {
    // A read by mistake spent the tree under INFRA-228, a root the rule picks.
    const spent = "INFRA-228";
    expect(pickedRoot(spent)).toBe(true);
    expect(reserveOf(treeCase("spent", LATER, spent))).toBe("kept");
    expect(reserveOf(treeCase("spent-at-opening", LATER, spent, PICKED))).toBe("kept");
    expect(reserveOf(treeCase("spent-at-build", LATER, PICKED, spent))).toBe("kept");
  });
});

describe("version 3: the list of trees the final test cannot take", () => {
  /** A case on a root version 2 does not pick, with each root's place in version 3's list. */
  function listedCase(
    caseId: string,
    openedAt: string,
    askRootListed: boolean | null | undefined,
    issueRootListed: boolean | null | undefined = askRootListed,
    root: string = NOT_PICKED
  ): CaseRow {
    const row: CaseRow = { ...treeCase(caseId, openedAt, root), askRootListed, issueRootListed };
    if (askRootListed === undefined) delete row.askRootListed;
    if (issueRootListed === undefined) delete row.issueRootListed;
    return row;
  }
  const atCut = RESERVE_LIST_CUT;
  const beforeCut = new Date(Date.parse(RESERVE_LIST_CUT) - 1).toISOString();

  test("from its cut, holds an ask unless both its roots are in the list, whatever version 2 picks", () => {
    expect(reserveOf(listedCase("both-out", atCut, false))).toBe("held");
    expect(reserveOf(listedCase("both-in", atCut, true))).toBe("kept");
    expect(reserveOf(listedCase("moved-out", atCut, true, false))).toBe("held");
    expect(reserveOf(listedCase("moved-in", atCut, false, true))).toBe("held");
    expect(reserveOf(listedCase("picked-in", atCut, true, true, PICKED))).toBe("kept");
  });

  test("from its cut, holds an ask whose root is unknown, or that a build never looked up", () => {
    expect(reserveOf({ ...listedCase("lost", atCut, null), askTreeRoot: null })).toBe("held");
    expect(reserveOf(listedCase("older-build", atCut, undefined))).toBe("held");
  });

  test("before its cut, version 2 holds as registered, less what opened on a tree in the list", () => {
    expect(reserveOf(listedCase("picked", beforeCut, false, false, PICKED))).toBe("held");
    expect(reserveOf(listedCase("not-picked", beforeCut, false))).toBe("kept");
    expect(reserveOf(listedCase("released", beforeCut, true, false, PICKED))).toBe("kept");
    // A move under a listed issue before a build releases nothing: it could move back after.
    expect(reserveOf(listedCase("moved-under-list", beforeCut, false, true, PICKED))).toBe("held");
  });

  test("a build that never looked roots up in the list releases nothing", () => {
    expect(reserveOf(listedCase("older-build", beforeCut, undefined, undefined, PICKED))).toBe(
      "held"
    );
    expect(reserveOf(listedCase("before-list", beforeCut, null, null, PICKED))).toBe("held");
  });

  test("keeps an ask of a tree a read spent, from its cut too", () => {
    expect(reserveOf(listedCase("spent", atCut, false, false, "INFRA-228"))).toBe("kept");
  });

  test("a build takes the list only at its registered sha256, and refuses any other", () => {
    const log = new TreeLog();
    const opened = (id: number, key: string, askId: string, at: string): DispatchEvent => ({
      id,
      issue_key: key,
      project: "INFRA",
      seq: id,
      type: "ask.opened",
      actor: null,
      created_at: at,
      payload: { id: askId, issue_key: key, created_at: at },
    });
    log.note(opened(1, "INFRA-1", "ask-old", "2026-11-15T23:00:00Z"));
    log.note(opened(2, PICKED, "ask-new", LATER));
    const roots = new Map([
      ["ask-old", "INFRA-1"],
      ["ask-new", PICKED],
    ]);
    const registered = createHash("sha256").update("INFRA-1\n", "utf8").digest("hex");
    const taken = takeReserveList(log, roots, registered);
    expect([...taken.listed]).toEqual(["INFRA-1"]);
    expect(taken.record.sha256).toBe(registered);
    expect(() => takeReserveList(log, roots, "0".repeat(64))).toThrow(CannotRunError);
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
const claimRunBefore = process.env[CLAIM_RUN_VARIABLE];

afterEach(async () => {
  for (const directory of made.splice(0)) await rm(directory, { recursive: true, force: true });
  if (variableBefore === undefined) delete process.env[RESERVE_VARIABLE];
  else process.env[RESERVE_VARIABLE] = variableBefore;
  if (claimRunBefore === undefined) delete process.env[CLAIM_RUN_VARIABLE];
  else process.env[CLAIM_RUN_VARIABLE] = claimRunBefore;
});

/**
 * A corpus of the given cases, written where `--corpus` reads one back from, with what its
 * `snapshot.json` records of the reserve, if anything (none, as a build before builds recorded it).
 */
async function corpusOnDisk(rows: readonly CaseRow[], reserve?: SnapshotReserve): Promise<string> {
  const directory = await mkdtemp(join(tmpdir(), "reserve-"));
  made.push(directory);
  await writeFile(
    join(directory, "snapshot.json"),
    `${JSON.stringify({ person: "someone", throughEventId: 1, reserve })}\n`,
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

  test("refuses a file that holds any case of a reserved tree, since its build counted the other cases' confounds with it", async () => {
    delete process.env[RESERVE_VARIABLE];
    for (const held of [reserved, sameIssueQuestion, child]) {
      const directory = await corpusOnDisk([open, held]);
      await expect(readCorpus(directory)).rejects.toThrow(
        `case ${held.caseId} is one the reserve holds, and while the reserve holds a read takes ` +
          "only a corpus whose build left every such case out"
      );
    }
  });

  test("reads them under a reference registered for this run, from a corpus built under it", async () => {
    process.env[RESERVE_VARIABLE] = `read:${claim}`;
    process.env[CLAIM_RUN_VARIABLE] = "start-1";
    const root = await bundleRoot();
    const run = await runDigestOf({ argv: process.argv, env: process.env }, root);
    const registration = { reads: [{ reference: claim, run }], ended: null };
    const builtUnder = {
      leftOut: false,
      cases: 1,
      issues: 1,
      readUnder: claim,
      claimRun: "start-1",
    };
    const directory = await corpusOnDisk([reserved, open], builtUnder);
    const { corpus, printed } = await read(directory, registration, root);
    expect(corpus.rows.map((row) => row.caseId)).toEqual(["reserved", "open"]);
    expect(corpus.reserve.readUnder).toBe(claim);
    expect(printed).toContain(`under ${claim}`);
  });

  test("refuses a read of the reserve over a corpus no build under that read wrote", async () => {
    process.env[RESERVE_VARIABLE] = `read:${claim}`;
    process.env[CLAIM_RUN_VARIABLE] = "start-1";
    const root = await bundleRoot();
    const run = await runDigestOf({ argv: process.argv, env: process.env }, root);
    const registration = { reads: [{ reference: claim, run }], ended: null };
    const another = { leftOut: false, cases: 1, issues: 1, readUnder: `${claim}-another` };
    for (const reserve of [undefined, another]) {
      const directory = await corpusOnDisk([reserved, open], reserve);
      await expect(readCorpus(directory, { registration, root })).rejects.toThrow(
        `under ${claim} takes only a corpus built under that read`
      );
    }
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

describe("the one claim run", () => {
  const claim = "dispatch://INFRA/artifact/infra-2897-final-test-reserve-md/comment/registration";
  const reply = (id: string, body: string, createdAt: string): CommentReply => ({
    id,
    body,
    created_at: createdAt,
    edited_at: null,
  });
  const first = reply("start-1", `${CLAIM_RUN_START} at the freeze`, "2026-11-22T10:00:00.000001Z");
  const second = reply("start-2", `  ${CLAIM_RUN_START} again`, "2026-11-22T12:00:00Z");
  const question = reply("question", "Which event pins the build?", "2026-11-22T09:00:00Z");
  const repliesOf =
    (...replies: CommentReply[]): RepliesOf =>
    async (commentId) => {
      expect(commentId).toBe("registration");
      return replies;
    };

  test("admits only the run the first start reply names, a resumed one included", async () => {
    const replies = repliesOf(second, question, first);
    const admitted = { kind: "claim", id: "start-1" } as const;
    expect(await claimRunOf(claim, "start-1", replies)).toEqual(admitted);
    expect(await claimRunOf(claim, " start-1 ", replies)).toEqual(admitted);
  });

  test("orders two starts in one millisecond by the microseconds Dispatch recorded", async () => {
    const later = reply("a-later", CLAIM_RUN_START, "2026-11-22T10:00:00.00012Z");
    const earlier = reply("z-earlier", CLAIM_RUN_START, "2026-11-22T10:00:00.000101Z");
    const replies = repliesOf(later, earlier);
    expect(await claimRunOf(claim, "z-earlier", replies)).toEqual({
      kind: "claim",
      id: "z-earlier",
    });
    expect((await claimRunOf(claim, "a-later", replies)).kind).toBe("refused");
    const untimed = reply("untimed", CLAIM_RUN_START, "not a time");
    const refused = await claimRunOf(claim, "z-earlier", repliesOf(earlier, untimed));
    expect(refused.kind).toBe("refused");
  });

  test("refuses every run once two starts share the earliest time, the one it admitted included", async () => {
    const at = "2026-11-22T10:00:00.000101Z";
    const alone = reply("z-first", CLAIM_RUN_START, at);
    expect(await claimRunOf(claim, "z-first", repliesOf(alone))).toEqual({
      kind: "claim",
      id: "z-first",
    });
    const tied = repliesOf(alone, reply("a-second", CLAIM_RUN_START, at));
    for (const named of ["z-first", "a-second"]) {
      const refused = await claimRunOf(claim, named, tied);
      expect(refused.kind).toBe("refused");
      if (refused.kind === "refused") expect(refused.reason).toContain(`have the same time, ${at}`);
    }
  });

  test("refuses a second claim run and any run that names no start, naming the first", async () => {
    const replies = repliesOf(first, question, second);
    for (const [named, why] of [
      ["start-2", "names start-2, a later start, at 2026-11-22T12:00:00Z"],
      ["question", "names question, which is no start reply to it"],
      [undefined, "APPROVAL_EVAL_CLAIM_RUN is not set"],
    ] as const) {
      const refused = await claimRunOf(claim, named, replies);
      expect(refused.kind).toBe("refused");
      if (refused.kind !== "refused") continue;
      expect(refused.reason).toContain(
        `the claim run under ${claim} is reply start-1, started at 2026-11-22T10:00:00.000001Z`
      );
      expect(refused.reason).toContain(why);
    }
  });

  test("refuses every run while no reply has started the claim run", async () => {
    const refused = await claimRunOf(claim, "question", repliesOf(question));
    expect(refused).toEqual({
      kind: "refused",
      reason:
        `no claim run has started under ${claim}: reply to it with a body that starts ` +
        `"${CLAIM_RUN_START}", then set ${CLAIM_RUN_VARIABLE} to that reply's id`,
    });
  });

  test("refuses every run once any reply to the registration was edited", async () => {
    const edited = { ...question, edited_at: "2026-11-22T13:00:00Z" };
    const refused = await claimRunOf(claim, "start-1", repliesOf(first, edited));
    expect(refused.kind).toBe("refused");
    if (refused.kind === "refused") expect(refused.reason).toContain("reply question");
  });

  test("counts a deleted reply as the start it may have been, so deleting the first start opens no second run", async () => {
    // The first start, deleted since: Dispatch serves it in its place with its mark and no body.
    const gone = { ...first, body: "", deleted_at: "2026-11-22T11:00:00Z" };
    for (const named of ["start-2", "start-1", undefined]) {
      const refused = await claimRunOf(claim, named, repliesOf(gone, question, second));
      expect(refused).toEqual({
        kind: "refused",
        reason:
          `reply start-1 to ${claim}, written at 2026-11-22T10:00:00.000001Z, before any start ` +
          "reply that is not deleted, was deleted at 2026-11-22T11:00:00Z: its words are gone, so " +
          "it may have started the claim run, whose read spent the final test, and no run reads " +
          "the reserve under this registration: claim.ts registers the read anew",
      });
    }
    // With no start standing, no claim run can start under the registration either.
    expect((await claimRunOf(claim, "start-2", repliesOf(gone))).kind).toBe("refused");
    // A reply deleted after the first start cannot be the first: the first start is the claim run.
    const later = {
      ...question,
      created_at: "2026-11-22T11:30:00Z",
      body: "",
      deleted_at: "2026-11-22T11:40:00Z",
    };
    const replies = repliesOf(first, later, second);
    expect(await claimRunOf(claim, "start-1", replies)).toEqual({ kind: "claim", id: "start-1" });
    const refused = await claimRunOf(claim, "question", replies);
    expect(refused.kind).toBe("refused");
    if (refused.kind === "refused") {
      expect(refused.reason).toContain(
        "names question, a later reply deleted at 2026-11-22T11:40:00Z"
      );
    }
  });

  test("refuses a reference that names no comment, and a thread Dispatch does not serve", async () => {
    expect(registrationCommentOf(claim)).toBe("registration");
    expect(registrationCommentOf("dispatch://INFRA-2897/comment/x#b-1")).toBeNull();
    expect(registrationCommentOf("dispatch://INFRA-2897")).toBeNull();
    const notComment = await claimRunOf("dispatch://INFRA-2897", "start-1", repliesOf(first));
    expect(notComment.kind).toBe("refused");
    const gone = await claimRunOf(claim, "start-1", async () => {
      throw new Error("GET /api/v1/comments/registration: 404 not found");
    });
    expect(gone.kind).toBe("refused");
    if (gone.kind === "refused") expect(gone.reason).toContain("404 not found");
  });

  test("a read of the reserve takes only the corpus its own claim run built", async () => {
    process.env[RESERVE_VARIABLE] = `read:${claim}`;
    const root = await bundleRoot();
    process.env[CLAIM_RUN_VARIABLE] = "start-1";
    const run = await runDigestOf({ argv: process.argv, env: process.env }, root);
    process.env[CLAIM_RUN_VARIABLE] = "start-2";
    expect(await runDigestOf({ argv: process.argv, env: process.env }, root)).toBe(run);
    const registration: Registration = { reads: [{ reference: claim, run }], ended: null };
    const reserved = caseRow("reserved", PICKED, LATER);
    const byFirst = { leftOut: false, cases: 1, issues: 1, readUnder: claim, claimRun: "start-1" };
    const directory = await corpusOnDisk([reserved], byFirst);
    await expect(readCorpus(directory, { registration, root })).rejects.toThrow(
      "was built by the claim run start-1 under"
    );
    delete process.env[CLAIM_RUN_VARIABLE];
    await expect(readCorpus(directory, { registration, root })).rejects.toThrow(
      "APPROVAL_EVAL_CLAIM_RUN is not set"
    );
    const { claimRun: _, ...beforeClaimRuns } = byFirst;
    const before = await corpusOnDisk([reserved], beforeClaimRuns);
    process.env[CLAIM_RUN_VARIABLE] = "start-1";
    await expect(readCorpus(before, { registration, root })).rejects.toThrow("of no record");
    const { corpus } = await read(directory, registration, root);
    expect(corpus.rows.map((row) => row.caseId)).toEqual(["reserved"]);
  });
});

describe("what a build writes of the reserve", () => {
  const reserved: CaseRow = {
    ...caseRow("reserved", PICKED, LATER),
    verdict: "request_changes",
    words: "not before the cache test",
    hasWords: true,
  };
  const child: CaseRow = {
    ...caseRow("child", "INFRA-4001", LATER),
    askTreeRoot: PICKED,
    issueTreeRoot: PICKED,
  };
  const open = caseRow("open", NOT_PICKED, LATER);
  const claim = "dispatch://INFRA-2897/comment/claim-run";
  const ended = "dispatch://INFRA-2897/comment/claim-result";
  const walked: Snapshot = {
    person: "someone",
    throughEventId: 1,
    eventsRead: 1,
    keptEvents: 1,
    readStartedAt: LATER,
    readFinishedAt: LATER,
    reserveList: null,
  };

  /** A corpus `writeCorpus` wrote under `access`: its directory, the rows it wrote, both files. */
  async function built(rows: readonly CaseRow[], access: Parameters<typeof writeCorpus>[3]) {
    const directory = await mkdtemp(join(tmpdir(), "built-"));
    made.push(directory);
    await writeCorpus(directory, rows, walked, access);
    const text = await readFile(join(directory, "corpus.jsonl"), "utf8");
    const snapshot = JSON.parse(await readFile(join(directory, "snapshot.json"), "utf8"));
    const written: CaseRow[] = text
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line));
    const ids = written.map((row) => row.caseId);
    return { directory, ids, written, text, reserve: snapshot.reserve };
  }

  test("counts a kept case's confounds without the cases it left out", async () => {
    const approval = {
      documents: [{ artifactId: "spec-1", name: "spec.md", version: 3, requestedVersion: 3 }],
      artifactSlug: "spec",
      artifactPrimary: true,
    };
    // A reserved request on the document asks for changes; a kept one, opened there before the
    // cut, is answered an hour later the same day.
    const first: CaseRow = {
      ...caseRow("first", PICKED, LATER),
      approval,
      verdict: "request_changes",
    };
    const kept: CaseRow = {
      ...caseRow("kept", PICKED, "2026-11-16T02:00:00Z"),
      approval,
      askCreatedAt: "2026-11-15T12:00:00Z",
    };
    const heldBuild = await built([first, kept], { kind: "held" });
    expect(heldBuild.ids).toEqual(["kept"]);
    expect(heldBuild.written[0]?.confounds).toMatchObject({
      changesAlreadyRequested: false,
      priorCasesOnDocument: 0,
      answersThatDayBefore: 0,
    });
    const claimBuild = await built([first, kept], {
      kind: "read",
      reference: claim,
      claimRun: "start-1",
    });
    expect(claimBuild.ids).toEqual(["first", "kept"]);
    expect(claimBuild.written[1]?.confounds).toMatchObject({
      changesAlreadyRequested: true,
      priorCasesOnDocument: 1,
      answersThatDayBefore: 1,
    });
  });

  test("while the reserve holds, writes none of its cases to corpus.jsonl, only their count", async () => {
    delete process.env[RESERVE_VARIABLE];
    const { directory, ids, text, reserve } = await built([reserved, child, open], {
      kind: "held",
    });
    expect(ids).toEqual(["open"]);
    expect(text).not.toContain(reserved.words ?? "");
    expect(reserve).toEqual({ leftOut: true, cases: 2, issues: 2, readUnder: null });
    const { corpus, printed } = await read(directory);
    expect(corpus.rows.map((row) => row.caseId)).toEqual(["open"]);
    expect(corpus.reserve).toEqual({ cases: 2, issues: 2, readUnder: null });
    expect(printed).toContain("held out 2 cases on 2 issues");
    expect(printed).toContain("left out of corpus.jsonl");
  });

  test("under a read claim.ts registers, writes them, and a read that holds the reserve refuses that corpus", async () => {
    delete process.env[RESERVE_VARIABLE];
    const { directory, ids, reserve } = await built([reserved, open], {
      kind: "read",
      reference: claim,
      claimRun: "start-1",
    });
    expect(ids).toEqual(["reserved", "open"]);
    expect(reserve).toEqual({
      leftOut: false,
      cases: 1,
      issues: 1,
      readUnder: claim,
      claimRun: "start-1",
    });
    await expect(readCorpus(directory)).rejects.toThrow(
      "case reserved is one the reserve holds, and while the reserve holds a read takes only a " +
        "corpus whose build left every such case out"
    );
  });

  test("once the reserve ended, writes every case and holds none", async () => {
    const { ids, reserve } = await built([reserved, open], { kind: "ended", record: ended });
    expect(ids).toEqual(["reserved", "open"]);
    expect(reserve).toEqual({ leftOut: false, cases: 0, issues: 0, readUnder: null });
  });

  test("refuses a read of the reserve over a corpus built while it held, and any read once it ended", async () => {
    const { directory } = await built([reserved, open], { kind: "held" });
    process.env[RESERVE_VARIABLE] = `read:${claim}`;
    process.env[CLAIM_RUN_VARIABLE] = "start-1";
    const root = await bundleRoot();
    const run = await runDigestOf({ argv: process.argv, env: process.env }, root);
    const registered: Registration = { reads: [{ reference: claim, run }], ended: null };
    await expect(readCorpus(directory, { registration: registered, root })).rejects.toThrow(
      /left the 1 case on 1 issue of INFRA-2897's next final test out of corpus\.jsonl/
    );
    delete process.env[RESERVE_VARIABLE];
    await expect(readCorpus(directory, { registration: { reads: [], ended } })).rejects.toThrow(
      /since the reserve ended .*: build it again/
    );
  });

  test("reads a corpus built while the reserve held that holds none of its cases, once it ended", async () => {
    const { directory } = await built([open], { kind: "held" });
    const { corpus } = await read(directory, { reads: [], ended });
    expect(corpus.rows.map((row) => row.caseId)).toEqual(["open"]);
  });

  test("refuses a file that holds a reserved case its snapshot says the build left out", async () => {
    delete process.env[RESERVE_VARIABLE];
    const { directory } = await built([open], { kind: "held" });
    await writeFile(
      join(directory, "corpus.jsonl"),
      `${JSON.stringify(reserved)}\n${JSON.stringify(open)}\n`,
      "utf8"
    );
    await expect(readCorpus(directory)).rejects.toThrow(/left every such case out of the file/);
  });

  test("refuses a snapshot that records the reserve in a shape no build writes", async () => {
    delete process.env[RESERVE_VARIABLE];
    const { directory } = await built([open], { kind: "held" });
    for (const reserve of [
      { leftOut: "yes", cases: 0, issues: 0, readUnder: null },
      { leftOut: true, cases: 1, issues: 1, readUnder: claim },
      { leftOut: false, cases: -1, issues: 0, readUnder: null },
      { leftOut: false, cases: 0, issues: 0 },
      { leftOut: false, cases: 0, issues: 0, readUnder: null, claimRun: "start-1" },
      { leftOut: false, cases: 1, issues: 1, readUnder: claim, claimRun: "" },
      { leftOut: false, cases: 1, issues: 1, readUnder: claim, claimRun: 7 },
    ]) {
      await writeFile(
        join(directory, "snapshot.json"),
        `${JSON.stringify({ ...walked, reserve })}\n`,
        "utf8"
      );
      await expect(readCorpus(directory)).rejects.toThrow(/shape no build writes/);
    }
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

  /** The ids readable from a corpus whose build left its one reserved case out and kept one open. */
  async function readable(): Promise<Map<string, CaseRow>> {
    delete process.env[RESERVE_VARIABLE];
    const open = caseRow("open", NOT_PICKED, LATER);
    const leftOut: SnapshotReserve = { leftOut: true, cases: 1, issues: 1, readUnder: null };
    return (await read(await corpusOnDisk([open], leftOut))).corpus.byCaseId;
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
