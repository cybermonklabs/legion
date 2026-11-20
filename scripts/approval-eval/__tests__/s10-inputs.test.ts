import { afterAll, beforeAll, describe, expect, spyOn, test } from "bun:test";
import { createHash } from "node:crypto";
import { existsSync } from "node:fs";
import { cp, mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { inspect } from "node:util";
import type { CaseRow } from "../build-corpus";
import type { DispatchActor, DispatchEvent } from "../dispatch-read";
import type { FinalTestTree } from "../draw-io";
import { LeakError } from "../render";
import { CLAIM_RUN_VARIABLE, RESERVE_VARIABLE, type Registration, runDigestOf } from "../reserve";
import {
  assertNoAnswer,
  type CandidateCounts,
  CandidateLog,
  InputsLog,
  type InventoryLine,
  inventoryOf,
  main,
  type RequestInputs,
  type ReserveVerdict,
  reserveAskDecision,
  S10InputsError,
  type StageRecord,
  writeTimeAsk,
} from "../s10-inputs";

/**
 * `s10-inputs.ts` against a fake Dispatch that serves the reads it makes in the shapes the real
 * routes answer: the event log (`/api/v1/events/log?after=&limit=`, rows with `id`, `type`,
 * `actor` and the payload, each ask payload with the fields the route adds when it is read), the
 * search (`/api/v1/search/results?q=&limit=&offset=`, `{results}`) and each document version
 * (`{markdown, sha256}`). The reserve's decision is handed in, as the command takes one, so no
 * `reserve-ask.ts` runs; the registration is one made for this test's own digest over a bundle
 * root of its own, as `readCorpus`'s tests make one.
 *
 * The log, in order: the issues, ACME-1's primary document doc-a and ACME-3's doc-b made with
 * them; neighbours on ACME-2 the person answered; the judged request ASK_A on ACME-1, opened,
 * edited twice, a comment on one neighbour, then the person's answer, an edit after it, a change
 * of every field of its issue, and a second answer; ASK_B on ACME-3, answered first by the
 * person's approval of its one document, then edited, then answered; ASK_C on ACME-4, whose last
 * edit before the person's answer carries another user's answer; a question ASK_Q and an approval
 * ASK_F the draw put in fit; a neighbour NOAT whose answer names no time, and ASK_G, whose search
 * finds it; ASK_D, naming ASK_A's document version under another name; ASK_E, naming a document
 * with no name; ASK_R, on which the person first restores another user's answer, then edits, then
 * answers; and, after the build's pin, a comment on the neighbour PINNED, which only the drop scan
 * reads. Every ask payload carries a primary flag that disagrees with its document's creation.
 */

const TOKEN = "fake-bearer";
const PERSON = "person@example.test";
const OTHER = "other@example.test";
const CLAIM = "dispatch://ACME-1/comment/registration";
const START = "start-1";
const SESSION: DispatchActor = { kind: "session", id: "session-1" };
const USER: DispatchActor = { kind: "user", id: PERSON };
const OTHER_USER: DispatchActor = { kind: "user", id: OTHER };

/** An ask id of the shape Dispatch gives one. */
const ask = (tail: string): string => `00000000-0000-4000-8000-${tail.padStart(12, "0")}`;
const ASK_A = ask("a1");
const ASK_D = ask("a2");
const ASK_E = ask("a3");
const ASK_G = ask("a4");
const ASK_R = ask("a5");
const ASK_B = ask("b1");
const ASK_C = ask("c1");
const ASK_Q = ask("d1");
const ASK_F = ask("f1");
const KEPT = ask("e1");
const LATER = ask("e2");
const HELD = ask("e3");
const UNSEEN = ask("e4");
const NEXT = ask("e5");
const PINNED = ask("e6");
const NOAT = ask("e7");

/** When event `id` was written: a minute apart, with Dispatch's microseconds. */
const at = (id: number): string =>
  new Date(Date.UTC(2026, 10, 17, 10) + id * 60_000).toISOString().replace(".000Z", ".000001Z");

const OPTIONS = [
  { id: "b0", label: "Approve", description: "Approve this version of the document." },
  { id: "b1", label: "Request changes", description: "Say what must change first." },
];
/** The options as the write stored them: the route adds each part id when it is read. */
const WRITTEN_OPTIONS = OPTIONS.map(({ label, description }) => ({ label, description }));

const DOCUMENTS: Record<string, string> = {
  "doc-a@2": "# The judged spec\n\nWhat the request asks to approve.\n",
  "doc-b@4": "# The settled spec\n\nA document the person approved directly.\n",
  "doc-g@1": "# A spec with one neighbour\n",
};

const sha256 = (text: string): string => createHash("sha256").update(text, "utf8").digest("hex");

/** One whole issue, as `issue.created` and `issue.updated` carry it. */
function issue(key: string, fields: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    key,
    title: `${key} title`,
    status: "in_progress",
    labels: [],
    parent: null,
    primary_artifact_id: null,
    ...fields,
  };
}

/**
 * One whole ask, as its opening, edits and answers carry it, with the fields the log's route adds
 * when it is read: the part ids, the opening's id, the counts, and the document as it stands now,
 * whose primary flag here is never the one its creation recorded.
 */
function whole(
  id: string,
  issueKey: string,
  opened: number,
  document: string,
  fields: Record<string, unknown> = {}
): Record<string, unknown> {
  const [artifact, version] = document.split("@");
  return {
    id,
    issue_key: issueKey,
    kind: "approval",
    state: "open",
    question: `The question of ${id}`,
    options: OPTIONS,
    multiple: false,
    answer: null,
    created_at: at(opened),
    opened_event_id: opened,
    referenced_by_count: 3,
    revision: 2,
    anchor_artifact: { project: "ACME", slug: "notes", name: "notes.md", primary: false },
    approval: { artifact_id: artifact, name: "spec.md", version: Number(version) },
    approval_artifact: { project: "ACME", slug: "spec", name: "spec.md", primary: false },
    ...fields,
  };
}

/** An answer, as `ask.answered` carries it on the whole ask. */
function answered(
  payload: Record<string, unknown>,
  id: number,
  selected: string[] = ["Approve"],
  text: string | null = null,
  user: string = PERSON
): Record<string, unknown> {
  return { ...payload, state: "answered", answer: { at: at(id), id: "b0", selected, text, user } };
}

function event(
  id: number,
  type: string,
  payload: Record<string, unknown>,
  actor: DispatchActor | null = SESSION
): DispatchEvent {
  const key = typeof payload.issue_key === "string" ? payload.issue_key : payload.key;
  return {
    id,
    issue_key: typeof key === "string" ? key : null,
    project: "ACME",
    seq: id,
    type,
    actor,
    created_at: at(id),
    payload,
  };
}

const A_OPENED = whole(ASK_A, "ACME-1", 16, "doc-a@2", { question: "First words" });
const B_OPENED = whole(ASK_B, "ACME-3", 24, "doc-b@4", { question: "Approve the settled spec?" });
const C_OPENED = whole(ASK_C, "ACME-4", 28, "doc-c@1");
const Q_OPENED = {
  ...whole(ASK_Q, "ACME-5", 33, "none@0"),
  kind: "question",
  approval: null,
  approval_artifact: null,
};
const F_OPENED = whole(ASK_F, "ACME-6", 35, "doc-f@1");
const G_OPENED = whole(ASK_G, "ACME-9", 43, "doc-g@1");
const D_OPENED = whole(ASK_D, "ACME-7", 45, "doc-a@2", {
  approval: { artifact_id: "doc-a", name: "renamed.md", version: 2 },
});
const E_OPENED = whole(ASK_E, "ACME-8", 47, "doc-e@1", {
  approval: {
    artifact_id: "doc-e",
    version: 1,
    documents: [{ artifact_id: "doc-e", version: 1 }],
  },
});
const R_OPENED = whole(ASK_R, "ACME-10", 49, "doc-r@1");
const neighbour = (id: string, opened: number): Record<string, unknown> =>
  whole(id, "ACME-2", opened, "doc-n@1", { question: `Neighbour question ${id}` });
/** NOAT as the person answered it: an answer that names no time. */
const NOAT_ANSWERED = {
  ...neighbour(NOAT, 41),
  state: "answered",
  answer: { selected: ["Approve"], text: null, user: PERSON },
};

const EVENTS: DispatchEvent[] = [
  event(
    1,
    "issue.created",
    issue("ACME-1", { title: "First title", status: "backlog", primary_artifact_id: "doc-a" })
  ),
  event(2, "issue.created", issue("ACME-2", { title: "Neighbour issue" })),
  event(3, "issue.created", issue("ACME-3", { primary_artifact_id: "doc-b" })),
  event(4, "issue.created", issue("ACME-4", { primary_artifact_id: "doc-c" })),
  event(5, "ask.opened", neighbour(PINNED, 5)),
  event(6, "ask.answered", answered(neighbour(PINNED, 5), 6), USER),
  event(7, "ask.opened", neighbour(KEPT, 7)),
  event(8, "ask.answered", answered(neighbour(KEPT, 7), 8, ["Approve"], "Fine by me"), USER),
  event(9, "ask.opened", neighbour(LATER, 9)),
  event(10, "ask.answered", answered(neighbour(LATER, 9), 10), USER),
  event(11, "ask.opened", neighbour(HELD, 11)),
  event(12, "ask.answered", answered(neighbour(HELD, 11), 12, ["Approve"], "held words"), USER),
  event(13, "ask.opened", neighbour(NEXT, 13)),
  event(14, "ask.answered", answered(neighbour(NEXT, 13), 14), USER),
  event(15, "issue.claimed", { key: "ACME-1", status: "in_progress" }),
  event(16, "ask.opened", A_OPENED),
  event(17, "ask.edited", { ...A_OPENED, question: "Second words" }),
  event(18, "ask.edited", { ...A_OPENED, question: "Third words" }),
  event(19, "comment.created", { ask_id: LATER, body: "a comment after the judged moment" }),
  event(20, "ask.answered", answered({ ...A_OPENED, question: "Third words" }, 20), USER),
  event(21, "ask.edited", { ...A_OPENED, question: "After the answer" }),
  event(
    22,
    "issue.updated",
    issue("ACME-1", {
      title: "Later title",
      status: "needs_review",
      labels: ["legion"],
      parent: "ACME-9",
      primary_artifact_id: "doc-z",
    })
  ),
  event(
    23,
    "ask.answered",
    answered({ ...A_OPENED, question: "After the answer" }, 23, ["Request changes"], "No."),
    USER
  ),
  event(24, "ask.opened", B_OPENED),
  event(25, "artifact.approved", { artifact_id: "doc-b", version: 4, ask_id: ASK_B }, USER),
  event(26, "ask.edited", { ...B_OPENED, question: "Edited after the approval" }),
  event(
    27,
    "ask.answered",
    answered({ ...B_OPENED, question: "Edited after the approval" }, 27),
    USER
  ),
  event(28, "ask.opened", C_OPENED),
  event(29, "ask.answered", answered(C_OPENED, 29, ["Approve"], null, OTHER), OTHER_USER),
  event(30, "ask.edited", answered(C_OPENED, 29, ["Approve"], null, OTHER)),
  event(31, "ask.answered", answered(C_OPENED, 31), USER),
  event(33, "ask.opened", Q_OPENED),
  event(34, "ask.answered", answered(Q_OPENED, 34), USER),
  event(35, "ask.opened", F_OPENED),
  event(36, "ask.answered", answered(F_OPENED, 36), USER),
  event(37, "issue.created", issue("ACME-7")),
  event(38, "issue.created", issue("ACME-8")),
  event(39, "issue.created", issue("ACME-9", { primary_artifact_id: "doc-g" })),
  event(40, "issue.created", issue("ACME-10")),
  event(41, "ask.opened", neighbour(NOAT, 41)),
  event(42, "ask.answered", NOAT_ANSWERED, USER),
  event(43, "ask.opened", G_OPENED),
  event(44, "ask.answered", answered(G_OPENED, 44), USER),
  event(45, "ask.opened", D_OPENED),
  event(46, "ask.answered", answered(D_OPENED, 46), USER),
  event(47, "ask.opened", E_OPENED),
  event(48, "ask.answered", answered(E_OPENED, 48), USER),
  event(49, "ask.opened", R_OPENED),
  // The person restores another user's answer: the event is the person's, the answer is not.
  event(50, "ask.answered", answered(R_OPENED, 50, ["Approve"], null, OTHER), USER),
  event(51, "ask.edited", { ...R_OPENED, question: "Edited after the restore" }),
  event(52, "ask.answered", answered(R_OPENED, 52), USER),
  // After the pin: the build's walk never reads it, and the drop scan reads its ask alone.
  event(60, "comment.created", { ask_id: PINNED, body: "a comment after the pin" }),
];
const THROUGH = 52;

/** The search's answers, by the query sent: hits in rank order. */
const documentHit = (index: number): Record<string, unknown> => ({
  kind: "document",
  id: `doc-hit-${index}`,
  rank: 0.5,
});
const askHit = (id: string, rank: number): Record<string, unknown> => ({
  kind: "ask",
  id,
  rank,
  ask: { state: "answered", answer: { user: PERSON } },
});
const SEARCH: Record<string, Record<string, unknown>[]> = {
  "First title": [askHit(KEPT, 0.9), documentHit(0), askHit(HELD, 0.7), askHit(PINNED, 0.6)],
  "Third words": [
    askHit(ASK_A, 0.95),
    askHit(LATER, 0.9),
    askHit(UNSEEN, 0.85),
    askHit(NEXT, 0.8),
    askHit(KEPT, 0.5),
  ],
  // Fifty hits fill the first page, so the second is asked for, and holds the one ask.
  "ACME-3 title": [
    ...Array.from({ length: 50 }, (_, index) => documentHit(index)),
    askHit(KEPT, 0.1),
  ],
  "ACME-9 title": [askHit(NOAT, 0.4)],
};

/** Each request the fake served, by its path and query. */
const seen: string[] = [];
/** How many walks of the log from its start the fake has served. */
let logWalks = 0;

let server: { stop(closeActiveConnections?: boolean): void | Promise<void> } | undefined;
let scratch: string | undefined;
let registration: Registration;
let root: string;
const before = {
  url: process.env.DISPATCH_URL,
  tokenFile: process.env.DISPATCH_TOKEN_FILE,
  reserve: process.env[RESERVE_VARIABLE],
  claimRun: process.env[CLAIM_RUN_VARIABLE],
};

beforeAll(async () => {
  const started = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const url = new URL(request.url);
      seen.push(`${url.pathname}${url.search}`);
      if (request.headers.get("authorization") !== `Bearer ${TOKEN}`) {
        return new Response("no bearer", { status: 401 });
      }
      if (url.pathname === "/api/v1/events/log") {
        const after = Number(url.searchParams.get("after"));
        const limit = Number(url.searchParams.get("limit"));
        if (after === 0) logWalks += 1;
        return Response.json(EVENTS.filter((one) => one.id > after).slice(0, limit));
      }
      if (url.pathname === "/api/v1/search/results") {
        const hits = SEARCH[url.searchParams.get("q") ?? ""] ?? [];
        const offset = Number(url.searchParams.get("offset"));
        const limit = Number(url.searchParams.get("limit"));
        return Response.json({ results: hits.slice(offset, offset + limit), total: hits.length });
      }
      const version = /^\/api\/v1\/artifacts\/([^/]+)\/versions\/(\d+)$/.exec(url.pathname);
      const markdown = version === null ? undefined : DOCUMENTS[`${version[1]}@${version[2]}`];
      if (markdown !== undefined) return Response.json({ markdown, sha256: sha256(markdown) });
      return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
    },
  });
  server = started;
  scratch = await mkdtemp(join(tmpdir(), "s10-inputs-"));
  const tokenFile = join(scratch, "token");
  await writeFile(tokenFile, `${TOKEN}\n`);
  process.env.DISPATCH_URL = `http://127.0.0.1:${started.port}`;
  process.env.DISPATCH_TOKEN_FILE = tokenFile;
  process.env[RESERVE_VARIABLE] = `read:${CLAIM}`;
  process.env[CLAIM_RUN_VARIABLE] = START;
  root = join(scratch, "bundle");
  await mkdir(join(root, "scripts/approval-eval"), { recursive: true });
  await writeFile(join(root, "scripts/approval-eval/run-round.ts"), "the arms\n", "utf8");
  const run = await runDigestOf({ argv: process.argv, env: process.env }, root);
  registration = { reads: [{ reference: CLAIM, run }], ended: null };
});

afterAll(async () => {
  await server?.stop(true);
  for (const [name, value] of [
    ["DISPATCH_URL", before.url],
    ["DISPATCH_TOKEN_FILE", before.tokenFile],
    [RESERVE_VARIABLE, before.reserve],
    [CLAIM_RUN_VARIABLE, before.claimRun],
  ] as const) {
    if (value === undefined) delete process.env[name];
    else process.env[name] = value;
  }
  if (scratch !== undefined) await rm(scratch, { recursive: true, force: true });
});

/** A case of the corpus, as the build writes one for the person's answer at event `id`. */
function caseRow(askId: string, id: number, kind: CaseRow["kind"], issueKey: string): CaseRow {
  return {
    caseId: `${askId}@${at(id)}`,
    eventId: id,
    askId,
    issueKey,
    project: "ACME",
    kind,
    urgency: "med",
    multiple: false,
    question: "The question",
    options: OPTIONS,
    selected: id === 23 ? ["Request changes"] : ["Approve"],
    words: null,
    hasWords: false,
    askCreatedAt: "2026-11-17T10:00:00Z",
    askTreeRoot: issueKey,
    issueTreeRoot: issueKey,
    askRootListed: true,
    issueRootListed: true,
    answeredAt: at(id),
    eventCreatedAt: at(id),
    approval: null,
    verdict: kind === "approval" ? "approve" : null,
    confounds: {
      version: null,
      answersThatDayBefore: 0,
      hourUtc: 10,
      hourPacific: 2,
      changesAlreadyRequested: false,
      priorCasesOnDocument: 0,
      caseKind: kind,
    },
  };
}

const A1 = caseRow(ASK_A, 20, "approval", "ACME-1");
const A2 = caseRow(ASK_A, 23, "approval", "ACME-1");
const B1 = caseRow(ASK_B, 27, "approval", "ACME-3");
const C1 = caseRow(ASK_C, 31, "approval", "ACME-4");
const Q1 = caseRow(ASK_Q, 34, "question", "ACME-5");
const F1 = caseRow(ASK_F, 36, "approval", "ACME-6");
const G1 = caseRow(ASK_G, 44, "approval", "ACME-9");
const D1 = caseRow(ASK_D, 46, "approval", "ACME-7");
const E1 = caseRow(ASK_E, 48, "approval", "ACME-8");
const R1 = caseRow(ASK_R, 52, "approval", "ACME-10");

const TREES: FinalTestTree[] = [
  { group: "tree:ACME-1", slice: "report", cases: [A1.caseId, A2.caseId].sort() },
  { group: "tree:ACME-10", slice: "report", cases: [R1.caseId] },
  { group: "tree:ACME-3", slice: "report", cases: [B1.caseId] },
  { group: "tree:ACME-4", slice: "report", cases: [C1.caseId] },
  { group: "tree:ACME-5", slice: "report", cases: [Q1.caseId] },
  { group: "tree:ACME-7", slice: "report", cases: [D1.caseId] },
  { group: "tree:ACME-8", slice: "report", cases: [E1.caseId] },
  { group: "tree:ACME-9", slice: "report", cases: [G1.caseId] },
];
const REPORT = TREES.flatMap((tree) => tree.cases);

/** A directory under this file's scratch directory. */
async function directory(name: string): Promise<string> {
  return mkdtemp(join(scratch as string, `${name}-`));
}

/** The sha256 of each of a build's three files, as a claim draw records them. */
interface BuildHashes {
  snapshotSha256: string;
  corpusSha256: string;
  treesSha256: string;
}

/** The claim run's build, with the sha256 of each of its three files. */
async function build(): Promise<{ corpus: string; hashes: BuildHashes }> {
  const corpus = await directory("build");
  const snapshot = `${JSON.stringify({
    person: PERSON,
    throughEventId: THROUGH,
    eventsRead: EVENTS.filter((one) => one.id <= THROUGH).length,
    keptEvents: 0,
    readStartedAt: "2026-11-20T00:00:00Z",
    readFinishedAt: "2026-11-20T00:01:00Z",
    reserve: { leftOut: false, cases: 0, issues: 0, readUnder: CLAIM, claimRun: START },
  })}\n`;
  const rows = [A1, A2, B1, C1, Q1, F1, G1, D1, E1, R1]
    .map((row) => `${JSON.stringify(row)}\n`)
    .join("");
  const trees = `${JSON.stringify({ version: 1, throughEventId: THROUGH, groups: [] })}\n`;
  await writeFile(join(corpus, "snapshot.json"), snapshot, "utf8");
  await writeFile(join(corpus, "corpus.jsonl"), rows, "utf8");
  await writeFile(join(corpus, "trees.json"), trees, "utf8");
  return {
    corpus,
    hashes: {
      snapshotSha256: sha256(snapshot),
      corpusSha256: sha256(rows),
      treesSha256: sha256(trees),
    },
  };
}

/** The claim run's draw of that build, with `draw.json` changed by `overrides`. */
async function drawOf(
  hashes: BuildHashes,
  overrides: Record<string, unknown> = {}
): Promise<string> {
  const draw = await directory("draw");
  const record = {
    corpus: "/the/build",
    cut: "2026-11-16T00:00:00.000Z",
    seed: 7,
    neverDecideApplied: false,
    claimRun: { id: START, ...hashes },
    finalTest: { reference: CLAIM, trees: TREES },
    ...overrides,
  };
  await writeFile(join(draw, "draw.json"), `${JSON.stringify(record)}\n`, "utf8");
  await writeFile(
    join(draw, "slices.json"),
    `${JSON.stringify({ fit: [F1.caseId], report: REPORT, anchor: [], reserve: [] })}\n`,
    "utf8"
  );
  return draw;
}

/** The reserve's decisions as a run was handed them, and what each was asked. */
interface Decisions {
  decide: (askId: string) => Promise<ReserveVerdict>;
  /** Each ask asked about, in order. */
  asked: string[];
  /** How many request files were written when each was asked. */
  filesWhenAsked: number[];
  /** How many walks of the log from its start had begun when each was asked. */
  walksWhenAsked: number[];
}

/** The reserve's decisions: HELD is held and every other ask kept. */
function decisions(out: () => string): Decisions {
  const asked: string[] = [];
  const filesWhenAsked: number[] = [];
  const walksWhenAsked: number[] = [];
  return {
    asked,
    filesWhenAsked,
    walksWhenAsked,
    decide: async (askId) => {
      asked.push(askId);
      walksWhenAsked.push(logWalks);
      const requests = join(out(), "requests");
      const files = existsSync(requests) ? await readdir(requests) : [];
      filesWhenAsked.push(files.filter((name) => name.endsWith(".json")).length);
      return askId === HELD ? "held" : "kept";
    },
  };
}

/** One run of the command, quiet, with its exit status and what it printed to stderr. */
async function run(
  argv: string[],
  decide: (askId: string) => Promise<ReserveVerdict>
): Promise<{ status: number; errors: string }> {
  const errors: string[] = [];
  const error = spyOn(console, "error").mockImplementation((line?: unknown) => {
    errors.push(String(line));
  });
  const log = spyOn(console, "log").mockImplementation(() => {});
  try {
    const status = await main(argv, { registration, root, decide });
    return { status, errors: errors.join("\n") };
  } finally {
    error.mockRestore();
    log.mockRestore();
  }
}

async function inventoryAt(out: string): Promise<InventoryLine[]> {
  const text = await readFile(join(out, "inventory.jsonl"), "utf8");
  return text
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as InventoryLine);
}

async function lineOf(out: string, askId: string): Promise<InventoryLine | undefined> {
  return (await inventoryAt(out)).find((line) => line.askId === askId);
}

async function requestAt(
  out: string,
  askId: string
): Promise<{ text: string; inputs: RequestInputs }> {
  const text = await readFile(join(out, "requests", `${askId}.json`), "utf8");
  return { text, inputs: JSON.parse(text) as RequestInputs };
}

async function stageAt(out: string): Promise<StageRecord> {
  return JSON.parse(await readFile(join(out, "s10-inputs.json"), "utf8")) as StageRecord;
}

/** Counts with nothing counted but what `counts` names. */
function countsWith(counts: Partial<CandidateCounts>): CandidateCounts {
  return {
    askHits: 0,
    candidates: 0,
    held: 0,
    reserveErrors: 0,
    droppedLaterEvent: { log: 0, "no-event-through-pin": 0, "after-pin": 0 },
    kept: 0,
    ...counts,
  };
}

describe("the claim's inputs, as of each request's judged moment", () => {
  let out: string;
  let corpus: string;
  let draw: string;
  let hashes: BuildHashes;
  let first: Decisions;
  let firstSeen: string[];
  let firstWalks: number;

  beforeAll(async () => {
    ({ corpus, hashes } = await build());
    draw = await drawOf(hashes);
    out = join(await directory("out"), "inputs");
    first = decisions(() => out);
    seen.length = 0;
    logWalks = 0;
    const { status, errors } = await run(
      ["--corpus", corpus, "--draw", draw, "--out", out],
      first.decide
    );
    firstSeen = [...seen];
    firstWalks = logWalks;
    expect(errors).not.toContain("S10InputsError");
    expect(status).toBe(0);
  });

  test("the inventory holds the answered approval requests of the report cases, and no other", async () => {
    const lines = await inventoryAt(out);
    expect(lines.map((line) => [line.askId, line.state, line.cases, line.group])).toEqual([
      [ASK_A, "ready", 2, "tree:ACME-1"],
      [ASK_D, "ready", 1, "tree:ACME-7"],
      [ASK_E, "deviation", 1, "tree:ACME-8"],
      [ASK_G, "deviation", 1, "tree:ACME-9"],
      [ASK_R, "deviation", 1, "tree:ACME-10"],
      [ASK_B, "ready", 1, "tree:ACME-3"],
      [ASK_C, "deviation", 1, "tree:ACME-4"],
    ]);
    // A case id carries the answer time, so no line names one.
    const text = await readFile(join(out, "inventory.jsonl"), "utf8");
    for (const row of [A1, A2, B1, C1, D1, E1, G1, R1]) expect(text).not.toContain(row.caseId);
  });

  test("an ask edited twice before the answer is judged at the second edit, not at an edit after it", async () => {
    expect((await lineOf(out, ASK_A))?.judged).toEqual({
      eventId: 18,
      type: "ask.edited",
      createdAt: at(18),
    });
    const { inputs } = await requestAt(out, ASK_A);
    expect(inputs.judged.eventId).toBe(18);
    expect(inputs.ask.question).toBe("Third words");
    expect(inputs.ask.answer).toBeNull();
    expect(inputs.search.question.query).toBe("Third words");
  });

  test("the issue is read as it stood at the judged moment, not as a later event left it", async () => {
    const { inputs } = await requestAt(out, ASK_A);
    expect(inputs.issue).toEqual({
      key: "ACME-1",
      title: "First title",
      status: "in_progress",
      labels: [],
      parent: null,
      primary_artifact_id: "doc-a",
    });
    expect(inputs.issueAsOfEventId).toBe(15);
    expect(inputs.search.title.query).toBe("First title");
  });

  test("the payload drops what the log's route adds when read, and the primary flag is the creation's", async () => {
    const { inputs } = await requestAt(out, ASK_A);
    for (const field of ["opened_event_id", "referenced_by_count", "revision", "anchor_artifact"]) {
      expect(inputs.ask).not.toHaveProperty(field);
    }
    expect(inputs.ask.options).toEqual(WRITTEN_OPTIONS);
    // The log serves `primary: false`; doc-a was made with ACME-1 as its primary document.
    expect(inputs.ask.approval_artifact).toEqual({ primary: true });
    const kept = inputs.candidates[KEPT] as { ask: Record<string, unknown> };
    expect(kept.ask.options).toEqual(WRITTEN_OPTIONS);
    expect(kept.ask.answer).toEqual({
      at: at(8),
      selected: ["Approve"],
      text: "Fine by me",
      user: PERSON,
    });
    expect(kept.ask).not.toHaveProperty("approval_artifact");
    expect(writeTimeAsk({ id: "x", revision: 3, answer: null })).toEqual({ id: "x", answer: null });
  });

  test("each document is the version the request names, under the name that request gives it", async () => {
    const markdown = DOCUMENTS["doc-a@2"] as string;
    const document = { artifact_id: "doc-a", version: 2, markdown, sha256: sha256(markdown) };
    expect((await requestAt(out, ASK_A)).inputs.documents).toEqual([
      { ...document, name: "spec.md" },
    ]);
    expect((await requestAt(out, ASK_D)).inputs.documents).toEqual([
      { ...document, name: "renamed.md" },
    ]);
    // The version was read once for both requests.
    const reads = firstSeen.filter((path) => path === "/api/v1/artifacts/doc-a/versions/2");
    expect(reads).toHaveLength(1);
  });

  test("a document the request names with no name makes it a deviation, naming the field", async () => {
    expect(await lineOf(out, ASK_E)).toMatchObject({
      state: "deviation",
      reason: "the log gives no approval.documents[0].name of the request as of event 47",
      requestSha256: null,
      counts: null,
    });
    expect(existsSync(join(out, "requests", `${ASK_E}.json`))).toBe(false);
  });

  test("a restored answer as the person's first answer event makes a deviation judged at nothing", async () => {
    const line = await lineOf(out, ASK_R);
    expect(line?.state).toBe("deviation");
    expect(line?.judged).toBeNull();
    expect(line?.reason).toContain(
      "first answer event on the ask carries no answer of the person's"
    );
  });

  test("no part of the person's answer to the judged request is written", async () => {
    const { text } = await requestAt(out, ASK_A);
    for (const answer of [A1, A2]) {
      expect(text).not.toContain(answer.caseId);
      expect(text).not.toContain(answer.answeredAt);
    }
    expect(text).not.toContain("After the answer");
    expect(text).not.toContain('"No."');
    expect(text).not.toContain("Later title");
    // No event id of the answer either: the answer is left to the analysis whole.
    expect(text).not.toMatch(/: 20,?\n/);
  });

  test("a candidate with an event after the judged moment is dropped, and the next hit is kept", async () => {
    const { inputs } = await requestAt(out, ASK_A);
    expect(inputs.search.question.hits.map((hit) => hit.id)).toEqual([
      ASK_A,
      LATER,
      UNSEEN,
      NEXT,
      KEPT,
    ]);
    expect(inputs.candidates[ASK_A]).toEqual({ dropped: "later-event", seen: "log" });
    expect(inputs.candidates[LATER]).toEqual({ dropped: "later-event", seen: "log" });
    expect(inputs.candidates[UNSEEN]).toEqual({
      dropped: "later-event",
      seen: "no-event-through-pin",
    });
    expect(inputs.candidates[NEXT]).toMatchObject({
      asOfEventId: 14,
      ask: { id: NEXT, state: "answered", answer: { user: PERSON, at: at(14) } },
      issue: { key: "ACME-2", title: "Neighbour issue" },
    });
    expect(inputs.candidates[KEPT]).toMatchObject({
      asOfEventId: 8,
      ask: { question: `Neighbour question ${KEPT}`, answer: { text: "Fine by me" } },
    });
    const counts = countsWith({
      askHits: 8,
      candidates: 7,
      held: 1,
      droppedLaterEvent: { log: 2, "no-event-through-pin": 1, "after-pin": 1 },
      kept: 2,
    });
    expect(inputs.counts).toEqual(counts);
    expect((await lineOf(out, ASK_A))?.counts).toEqual(counts);
  });

  test("a candidate whose only later event is after the pin is dropped, with none of its fields", async () => {
    const { text, inputs } = await requestAt(out, ASK_A);
    expect(inputs.candidates[PINNED]).toEqual({ dropped: "later-event", seen: "after-pin" });
    expect(text).not.toContain(`Neighbour question ${PINNED}`);
    expect((await stageAt(out)).dropScanThrough).toBe(60);
    // The scan read on from the pin, and the walks that gave the inputs stopped at it.
    expect(firstSeen).toContain(`/api/v1/events/log?after=${THROUGH}&limit=1000`);
  });

  test("a candidate S10 would read with no answer time makes a deviation that keeps its counts", async () => {
    expect(await lineOf(out, ASK_G)).toEqual({
      askId: ASK_G,
      issueKey: "ACME-9",
      group: "tree:ACME-9",
      cases: 1,
      judged: { eventId: 43, type: "ask.opened", createdAt: at(43) },
      state: "deviation",
      reason:
        `a candidate: the log gives no answer.at of the ask ${NOAT} as of event 43, which ` +
        "S10's neighbours reads",
      requestSha256: null,
      counts: countsWith({ askHits: 1, candidates: 1, kept: 1 }),
    });
  });

  test("a neighbour the reserve holds is skipped, and no field of it is ever read", async () => {
    const { text, inputs } = await requestAt(out, ASK_A);
    expect(inputs.candidates[HELD]).toEqual({ held: true, reserve: "held" });
    expect(text).not.toContain(`Neighbour question ${HELD}`);
    expect(text).not.toContain("held words");
    // Every ask hit was decided after the first walk and before the second began, and before any
    // request file was written.
    expect([...new Set(first.asked)].sort()).toEqual(
      [ASK_A, KEPT, LATER, HELD, UNSEEN, NEXT, PINNED, NOAT].sort()
    );
    expect(first.walksWhenAsked.every((walks) => walks === 1)).toBe(true);
    expect(firstWalks).toBe(2);
    expect(first.filesWhenAsked.every((count) => count === 0)).toBe(true);
  });

  test("the first walk keeps no field of an ask outside the inventory, the second none of a held one", () => {
    const pinned = EVENTS.filter((one) => one.id <= THROUGH);
    const log = new InputsLog(PERSON, [ASK_A]);
    for (const one of pinned) log.note(one);
    const held = inspect(log, { depth: Number.POSITIVE_INFINITY });
    expect(held).not.toContain("Neighbour question");
    expect(held).not.toContain("held words");
    expect(held).not.toContain("Fine by me");
    expect(log.lastEventOf(HELD)).toBe(12);
    const candidates = new CandidateLog(new Set([KEPT]));
    for (const one of pinned) candidates.note(one);
    expect(candidates.latestOf(HELD)).toBeUndefined();
    expect(candidates.latestOf(KEPT)?.eventId).toBe(8);
    expect(inspect(candidates, { depth: Number.POSITIVE_INFINITY })).not.toContain("held words");
  });

  test("a request of one document is answered by the person's approval of it", async () => {
    expect((await lineOf(out, ASK_B))?.judged).toEqual({
      eventId: 24,
      type: "ask.opened",
      createdAt: at(24),
    });
    const { inputs } = await requestAt(out, ASK_B);
    expect(inputs.ask.question).toBe("Approve the settled spec?");
    expect(inputs.ask.approval_artifact).toEqual({ primary: true });
  });

  test("a payload that carries an answer at the judged moment makes the request a deviation", async () => {
    expect(await lineOf(out, ASK_C)).toMatchObject({
      state: "deviation",
      reason: "the payload at the judged moment carries an answer",
      requestSha256: null,
    });
    expect(existsSync(join(out, "requests", `${ASK_C}.json`))).toBe(false);
  });

  test("a second page is read only where the first held fifty results, and each page once", async () => {
    const searched = firstSeen.filter((path) => path.startsWith("/api/v1/search/results"));
    expect(searched.filter((path) => path.includes("offset=50"))).toEqual([
      `/api/v1/search/results?q=${encodeURIComponent("ACME-3 title")}&limit=50&offset=50`,
    ]);
    // Two queries each of A, B, D and G, B's title at two pages.
    expect(searched).toHaveLength(9);
    expect(new Set(searched).size).toBe(9);
    const { inputs } = await requestAt(out, ASK_B);
    expect(inputs.search.title.pages.map((page) => page.offset)).toEqual([0, 50]);
    expect(inputs.search.title.hits).toEqual([
      { id: KEPT, rank: 0.1, page: inputs.search.title.pages[1]?.sha256, position: 0 },
    ]);
  });

  test("each page is kept as its hits and the sha256 of its bytes, and never its body", async () => {
    const pages = await readdir(join(out, "pages"));
    // Four queries were answered with the same empty page, which is one record of four reads.
    expect(pages).toHaveLength(6);
    const reads: number[] = [];
    for (const name of pages) {
      const text = await readFile(join(out, "pages", name), "utf8");
      const page = JSON.parse(text);
      expect(name).toBe(`${page.sha256}.json`);
      expect(Object.keys(page).sort()).toEqual(["hits", "reads", "sha256"]);
      expect(text).not.toContain('"answer"');
      reads.push(page.reads.length);
    }
    expect(reads.sort()).toEqual([1, 1, 1, 1, 1, 4]);
  });

  test("the stage record chains the build, the draw and every file written, with summed counts", async () => {
    const record = await stageAt(out);
    expect(record).toMatchObject({
      stage: "s10-inputs",
      state: "written",
      claimRun: START,
      reference: CLAIM,
      person: PERSON,
      throughEventId: THROUGH,
      build: hashes,
      draw: {
        drawSha256: sha256(await readFile(join(draw, "draw.json"), "utf8")),
        slicesSha256: sha256(await readFile(join(draw, "slices.json"), "utf8")),
      },
      reserveAsk: null,
      inventorySha256: sha256(await readFile(join(out, "inventory.jsonl"), "utf8")),
      dropScanThrough: 60,
    });
    expect(record.requests).toEqual({
      [ASK_A]: sha256((await requestAt(out, ASK_A)).text),
      [ASK_B]: sha256((await requestAt(out, ASK_B)).text),
      [ASK_D]: sha256((await requestAt(out, ASK_D)).text),
    });
    for (const [page, sha] of Object.entries(record.pages)) {
      expect(sha256(await readFile(join(out, "pages", `${page}.json`), "utf8"))).toBe(sha);
    }
    expect(record.counts).toEqual({
      ...countsWith({
        askHits: 10,
        candidates: 9,
        held: 1,
        droppedLaterEvent: { log: 2, "no-event-through-pin": 1, "after-pin": 1 },
        kept: 4,
      }),
      requests: 7,
      ready: 3,
      deviations: 4,
      cases: 8,
      pages: 6,
    });
  });

  test("a resume of a complete directory takes every file, its first scan, and the same counts", async () => {
    const before = await stageAt(out);
    const files = await Promise.all(
      [ASK_A, ASK_B, ASK_D].map(async (askId) => (await requestAt(out, askId)).text)
    );
    // An event of NOAT after the first run's scan: a resume scans through that run's last event
    // and no further, so NOAT is not dropped and G stays the deviation it was.
    EVENTS.push(event(61, "comment.created", { ask_id: NOAT, body: "after the first scan" }));
    const again = decisions(() => out);
    seen.length = 0;
    try {
      const { status } = await run(
        ["--corpus", corpus, "--draw", draw, "--out", out],
        again.decide
      );
      expect(status).toBe(0);
    } finally {
      EVENTS.pop();
    }
    expect(seen.filter((path) => path.startsWith("/api/v1/search/results"))).toEqual([]);
    const after = await Promise.all(
      [ASK_A, ASK_B, ASK_D].map(async (askId) => (await requestAt(out, askId)).text)
    );
    expect(after).toEqual(files);
    const record = await stageAt(out);
    expect(record.dropScanThrough).toBe(60);
    expect(record.requests).toEqual(before.requests);
    expect(record.counts).toEqual(before.counts);
    expect((await lineOf(out, ASK_G))?.reason).toContain(`no answer.at of the ask ${NOAT}`);
    // Only the request with no file was worked on again.
    expect(again.asked).toEqual([NOAT]);
  });

  /** A copy of the inputs, changed by `change`, resumed: its exit status and its refusal. */
  async function resumedCopy(
    change: (copy: string) => Promise<void>
  ): Promise<{ status: number; errors: string }> {
    const copy = join(await directory("copy"), "inputs");
    await cp(out, copy, { recursive: true });
    await change(copy);
    return run(["--corpus", corpus, "--draw", draw, "--out", copy], decisions(() => copy).decide);
  }

  test("a resume refuses a request file its record names that is gone, and never writes it again", async () => {
    const path = (copy: string): string => join(copy, "requests", `${ASK_A}.json`);
    const refused = await resumedCopy((copy) => rm(path(copy)));
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain("which is not there: a resume never writes a file");
  });

  test("a resume refuses a request file whose bytes are not the ones its record names", async () => {
    const refused = await resumedCopy(async (copy) => {
      const path = join(copy, "requests", `${ASK_A}.json`);
      const inputs = JSON.parse(await readFile(path, "utf8"));
      inputs.ask.answer = { user: PERSON, selected: ["Request changes"], text: "No.", at: at(20) };
      await writeFile(path, `${JSON.stringify(inputs, null, 2)}\n`, "utf8");
    });
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain("is not the file the stage record names");
  });

  test("a resume refuses a file its record does not name", async () => {
    const refused = await resumedCopy(async (copy) => {
      const { text } = await requestAt(copy, ASK_A);
      await writeFile(join(copy, "requests", `${ASK_C}.json`), text, "utf8");
    });
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain("is a file the stage record does not name");
  });

  test("a resume refuses an --out holding another claim run's inputs, or files of no record", async () => {
    const other = join(await directory("other"), "inputs");
    await mkdir(other, { recursive: true });
    const record = await stageAt(out);
    await writeFile(
      join(other, "s10-inputs.json"),
      JSON.stringify({ ...record, claimRun: "start-0" }),
      "utf8"
    );
    const argv = ["--corpus", corpus, "--draw", draw, "--out", other];
    const refused = await run(argv, decisions(() => other).decide);
    expect(refused.status).toBe(2);
    expect(refused.errors).toContain("holds another run's inputs: its claim run is start-0");
    const stray = join(await directory("stray"), "inputs");
    await mkdir(stray, { recursive: true });
    await writeFile(join(stray, "notes.txt"), "not this run's\n", "utf8");
    const strayRun = await run(
      ["--corpus", corpus, "--draw", draw, "--out", stray],
      decisions(() => stray).decide
    );
    expect(strayRun.status).toBe(2);
    expect(strayRun.errors).toContain("holds files and no s10-inputs.json");
  });

  test("refuses a draw from another claim run, from another build, or one that applied the rule", async () => {
    const cases: Array<[Record<string, unknown>, string]> = [
      [{ claimRun: { ...hashes, id: "start-2" } }, "was drawn in the claim run start-2"],
      [
        { claimRun: { ...hashes, id: START, snapshotSha256: "b".repeat(64) } },
        "was drawn from the build whose snapshot.json has sha256",
      ],
      [{ claimRun: null }, "was drawn in the claim run of no record"],
      [{ neverDecideApplied: true }, "does not record that it applied no never-decide rule"],
    ];
    for (const [overrides, words] of cases) {
      const other = await drawOf(hashes, overrides);
      const target = join(await directory("refused"), "inputs");
      const refused = await run(
        ["--corpus", corpus, "--draw", other, "--out", target],
        decisions(() => target).decide
      );
      expect(refused.status).toBe(2);
      expect(refused.errors).toContain(words);
      expect(existsSync(target)).toBe(false);
    }
  });

  test("refuses a build whose corpus.jsonl or trees.json changed under the same snapshot", async () => {
    for (const file of ["corpus.jsonl", "trees.json"]) {
      const changed = await directory("changed");
      await cp(corpus, changed, { recursive: true });
      await writeFile(
        join(changed, file),
        `${await readFile(join(corpus, file), "utf8")}\n`,
        "utf8"
      );
      const target = join(await directory("refused"), "inputs");
      const refused = await run(
        ["--corpus", changed, "--draw", draw, "--out", target],
        decisions(() => target).decide
      );
      expect(refused.status).toBe(2);
      expect(refused.errors).toContain(`was drawn from the build whose ${file} has sha256`);
      expect(existsSync(target)).toBe(false);
    }
  });

  test("refuses to run outside the registered read", async () => {
    const target = join(await directory("unread"), "inputs");
    process.env[RESERVE_VARIABLE] = "";
    try {
      const refused = await run(
        ["--corpus", corpus, "--draw", draw, "--out", target],
        decisions(() => target).decide
      );
      expect(refused.status).toBe(2);
      expect(refused.errors).toContain("written only under its registered read");
    } finally {
      process.env[RESERVE_VARIABLE] = `read:${CLAIM}`;
    }
  });
});

describe("the inventory", () => {
  const byCaseId = new Map([A1, A2, B1, Q1].map((row) => [row.caseId, row]));
  const trees: FinalTestTree[] = [
    { group: "tree:ACME-1", slice: "report", cases: [A1.caseId, A2.caseId].sort() },
    { group: "tree:ACME-3", slice: "report", cases: [B1.caseId] },
    { group: "tree:ACME-5", slice: "report", cases: [Q1.caseId] },
  ];
  const report = [A1.caseId, A2.caseId, B1.caseId, Q1.caseId];

  test("maps the cases of one ask to one request, and leaves the questions out", () => {
    const inventory = inventoryOf(byCaseId, trees, report);
    expect(inventory.map((request) => [request.askId, request.rows.length])).toEqual([
      [ASK_A, 2],
      [ASK_B, 1],
    ]);
  });

  test("refuses a case named twice, and an ask whose cases sit in two trees", () => {
    expect(() => inventoryOf(byCaseId, trees, [...report, B1.caseId])).toThrow(
      `the draw's report slice names the case ${B1.caseId} twice`
    );
    const split: FinalTestTree[] = [
      { group: "tree:ACME-1", slice: "report", cases: [A1.caseId] },
      { group: "tree:ACME-7", slice: "report", cases: [A2.caseId] },
      ...trees.slice(1),
    ];
    expect(() => inventoryOf(byCaseId, split, report)).toThrow(
      `the ask ${ASK_A} has cases in the trees tree:ACME-1 and tree:ACME-7`
    );
  });

  test("refuses a report case no tree holds, and a tree's case the report slice lacks", () => {
    expect(() => inventoryOf(byCaseId, trees.slice(0, 2), report)).toThrow(
      "which no eligible tree of its record holds"
    );
    expect(() => inventoryOf(byCaseId, trees, report.slice(1))).toThrow(S10InputsError);
    const unknown = [...report, "nowhere@2026-11-17T10:00:00Z"];
    expect(() => inventoryOf(byCaseId, trees, unknown)).toThrow("which the build does not hold");
  });
});

describe("the guard over a request file", () => {
  const inputs = (markdown: string): RequestInputs => ({
    version: 2,
    askId: ASK_A,
    issueKey: "ACME-1",
    group: "tree:ACME-1",
    cases: 1,
    judged: { eventId: 18, type: "ask.edited", createdAt: at(18) },
    ask: { id: ASK_A, answer: null, question: "Third words" },
    issue: {
      key: "ACME-1",
      title: "First title",
      status: "backlog",
      labels: [],
      parent: null,
      primary_artifact_id: null,
    },
    issueAsOfEventId: 15,
    documents: [{ artifact_id: "doc-a", name: "spec.md", version: 2, markdown, sha256: "x" }],
    search: {
      title: { query: "First title", pages: [], hits: [] },
      question: { query: "Third words", pages: [], hits: [] },
    },
    candidates: {},
    counts: {
      askHits: 0,
      candidates: 0,
      held: 0,
      reserveErrors: 0,
      droppedLaterEvent: { log: 0, "no-event-through-pin": 0, "after-pin": 0 },
      kept: 0,
    },
  });
  const guard = {
    askId: ASK_A,
    answerEventId: 20,
    selected: [["Approve"]],
    judgedAt: 1_000 * Date.parse(at(18)) + 1,
  };

  test("lets a request with no part of its answer through", () => {
    expect(assertNoAnswer(inputs("# The judged spec\n"), guard)).toContain('"Third words"');
  });

  test("refuses the judged ask's fence carrying an answer, or an answer dated after the moment", () => {
    const fence = `:::ask{#${ASK_A} state="answered" selected="[&#x22;Approve&#x22;]"}`;
    expect(() => assertNoAnswer(inputs(`${fence}\n`), guard)).toThrow(LeakError);
    const later = `:::ask{#other answered_at="${at(19)}" state="answered"}`;
    expect(() => assertNoAnswer(inputs(`${later}\n`), guard)).toThrow(
      "an answer dated at or after the judged moment"
    );
  });

  test("refuses a field naming the answer's event, and a case id of the judged ask", () => {
    const withEvent = { ...inputs(""), issueAsOfEventId: 20 };
    expect(() => assertNoAnswer(withEvent, guard)).toThrow("the id of the person's answer event");
    const withCase = { ...inputs(""), group: `${ASK_A}@${at(20)}` };
    expect(() => assertNoAnswer(withCase, guard)).toThrow("a case id of the judged ask");
  });
});

describe("the reserve's decision", () => {
  test("refuses a reserve-ask.ts whose bytes are not commit 651db8d5's", async () => {
    const path = join(await directory("reserve-ask"), "reserve-ask.ts");
    await writeFile(path, "console.log('another rule');\n", "utf8");
    await expect(reserveAskDecision(path)).rejects.toThrow(S10InputsError);
    await expect(reserveAskDecision(path)).rejects.toThrow("651db8d5");
  });
});
