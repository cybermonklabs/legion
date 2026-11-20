import { afterAll, beforeAll, describe, expect, spyOn, test } from "bun:test";
import { createHash } from "node:crypto";
import { existsSync } from "node:fs";
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { CaseRow } from "../build-corpus";
import type { DispatchActor, DispatchEvent } from "../dispatch-read";
import type { FinalTestTree } from "../draw-io";
import { LeakError } from "../render";
import {
  CLAIM_RUN_VARIABLE,
  type Registration,
  RESERVE_VARIABLE,
  runDigestOf,
} from "../reserve";
import {
  assertNoAnswer,
  type InventoryLine,
  inventoryOf,
  main,
  type RequestInputs,
  type ReserveVerdict,
  reserveAskDecision,
  S10InputsError,
  type StageRecord,
} from "../s10-inputs";

/**
 * `s10-inputs.ts` against a fake Dispatch that serves the reads it makes in the shapes the real
 * routes answer: the event log (`/api/v1/events/log?after=&limit=`, rows with `id`, `type`,
 * `actor` and the payload), the search (`/api/v1/search/results?q=&limit=&offset=`, `{results}`)
 * and each document version (`{markdown, sha256}`). The reserve's decision is handed in, as the
 * command takes one, so no `reserve-ask.ts` runs; the registration is one made for this test's
 * own digest over a bundle root of its own, as `readCorpus`'s tests make one.
 *
 * The log, in order: the issues; five neighbours on ACME-2 the person answered; the judged
 * request ASK_A on ACME-1, opened, edited twice, a comment on one neighbour, then the person's
 * answer, an edit after it, a change of every field of its issue, and a second answer; ASK_B on
 * ACME-3, answered first by the person's approval of its one document, then edited, then answered;
 * ASK_C on ACME-4, whose last edit before the person's answer carries another user's answer; a
 * question ASK_Q on ACME-5 and an approval ASK_F on ACME-6 the draw put in fit; and, after the
 * build's pin, a comment on the neighbour PINNED, which only the drop scan reads.
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

/** When event `id` was written: a minute apart, with Dispatch's microseconds. */
const at = (id: number): string => `2026-11-17T10:${String(id).padStart(2, "0")}:00.000001Z`;

const OPTIONS = [
  { id: "b0", label: "Approve", description: "Approve this version of the document." },
  { id: "b1", label: "Request changes", description: "Say what must change first." },
];

const DOCUMENTS: Record<string, string> = {
  "doc-a@2": "# The judged spec\n\nWhat the request asks to approve.\n",
  "doc-b@4": "# The settled spec\n\nA document the person approved directly.\n",
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

/** One whole ask, as its opening, edits and answers carry it. */
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
    approval: { artifact_id: artifact, name: "spec.md", version: Number(version) },
    approval_artifact: { project: "ACME", slug: "spec", name: "spec.md", primary: true },
    ...fields,
  };
}

/** The person's answer, as `ask.answered` carries it on the whole ask. */
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
const neighbour = (id: string, opened: number): Record<string, unknown> =>
  whole(id, "ACME-2", opened, "doc-n@1", { question: `Neighbour question ${id}` });

const EVENTS: DispatchEvent[] = [
  event(1, "issue.created", issue("ACME-1", { title: "First title", status: "backlog" })),
  event(2, "issue.created", issue("ACME-2", { title: "Neighbour issue" })),
  event(3, "issue.created", issue("ACME-3")),
  event(4, "issue.created", issue("ACME-4")),
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
  // After the pin: the build's walk never reads it, and the drop scan reads its ask alone.
  event(40, "comment.created", { ask_id: PINNED, body: "a comment after the pin" }),
];
const THROUGH = 36;

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
};

/** Each request the fake served, by its path and query. */
const seen: string[] = [];

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

const TREES: FinalTestTree[] = [
  { group: "tree:ACME-1", slice: "report", cases: [A1.caseId, A2.caseId].sort() },
  { group: "tree:ACME-3", slice: "report", cases: [B1.caseId] },
  { group: "tree:ACME-4", slice: "report", cases: [C1.caseId] },
  { group: "tree:ACME-5", slice: "report", cases: [Q1.caseId] },
];
const REPORT = [A1.caseId, A2.caseId, B1.caseId, C1.caseId, Q1.caseId];

/** A directory under this file's scratch directory. */
async function directory(name: string): Promise<string> {
  const path = await mkdtemp(join(scratch as string, `${name}-`));
  return path;
}

/** The claim run's build, with what its `snapshot.json` holds and the sha256 of its bytes. */
async function build(): Promise<{ corpus: string; snapshotSha256: string }> {
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
  await writeFile(join(corpus, "snapshot.json"), snapshot, "utf8");
  const rows = [A1, A2, B1, C1, Q1, F1].map((row) => `${JSON.stringify(row)}\n`).join("");
  await writeFile(join(corpus, "corpus.jsonl"), rows, "utf8");
  return { corpus, snapshotSha256: sha256(snapshot) };
}

/** The claim run's draw of that build, with `draw.json` changed by `overrides`. */
async function drawOf(
  snapshotSha256: string,
  overrides: Record<string, unknown> = {}
): Promise<string> {
  const draw = await directory("draw");
  const record = {
    corpus: "/the/build",
    cut: "2026-11-16T00:00:00.000Z",
    seed: 7,
    neverDecideApplied: false,
    claimRun: { id: START, snapshotSha256 },
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
}

/** The reserve's decisions: HELD is held and every other ask kept. */
function decisions(out: () => string): Decisions {
  const asked: string[] = [];
  const filesWhenAsked: number[] = [];
  return {
    asked,
    filesWhenAsked,
    decide: async (askId) => {
      asked.push(askId);
      const requests = join(out(), "requests");
      filesWhenAsked.push(existsSync(requests) ? (await readdir(requests)).length : 0);
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

async function requestAt(
  out: string,
  askId: string
): Promise<{ text: string; inputs: RequestInputs }> {
  const text = await readFile(join(out, "requests", `${askId}.json`), "utf8");
  return { text, inputs: JSON.parse(text) as RequestInputs };
}

describe("the claim's inputs, as of each request's judged moment", () => {
  let out: string;
  let corpus: string;
  let draw: string;
  let snapshotSha256: string;
  let first: Decisions;

  beforeAll(async () => {
    ({ corpus, snapshotSha256 } = await build());
    draw = await drawOf(snapshotSha256);
    out = join(await directory("out"), "inputs");
    first = decisions(() => out);
    seen.length = 0;
    const { status, errors } = await run(
      ["--corpus", corpus, "--draw", draw, "--out", out],
      first.decide
    );
    expect(errors).not.toContain("S10InputsError");
    expect(status).toBe(0);
  });

  test("the inventory holds the answered approval requests of the report cases, and no other", async () => {
    const lines = await inventoryAt(out);
    expect(lines.map((line) => line.askId)).toEqual([ASK_A, ASK_B, ASK_C]);
    expect(lines.map((line) => [line.state, line.cases, line.group])).toEqual([
      ["ready", 2, "tree:ACME-1"],
      ["ready", 1, "tree:ACME-3"],
      ["deviation", 1, "tree:ACME-4"],
    ]);
    // A case id carries the answer time, so no line names one.
    const text = await readFile(join(out, "inventory.jsonl"), "utf8");
    for (const row of [A1, A2, B1, C1]) expect(text).not.toContain(row.caseId);
  });

  test("an ask edited twice before the answer is judged at the second edit, not at an edit after it", async () => {
    const [lineA] = await inventoryAt(out);
    expect(lineA?.judged).toEqual({ eventId: 18, type: "ask.edited", createdAt: at(18) });
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
      primary_artifact_id: null,
    });
    expect(inputs.issueAsOfEventId).toBe(15);
    expect(inputs.search.title.query).toBe("First title");
  });

  test("each document is the version the request names, checked against its sha256", async () => {
    const { inputs } = await requestAt(out, ASK_A);
    const markdown = DOCUMENTS["doc-a@2"] as string;
    expect(inputs.documents).toEqual([
      { artifact_id: "doc-a", name: "spec.md", version: 2, markdown, sha256: sha256(markdown) },
    ]);
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
    expect(inputs.counts).toEqual({
      askHits: 8,
      candidates: 7,
      held: 1,
      reserveErrors: 0,
      droppedLaterEvent: 4,
      kept: 2,
    });
  });

  test("a candidate whose only later event is after the pin is dropped, with none of its fields", async () => {
    const { text, inputs } = await requestAt(out, ASK_A);
    expect(inputs.candidates[PINNED]).toEqual({ dropped: "later-event", seen: "after-pin" });
    expect(text).not.toContain(`Neighbour question ${PINNED}`);
    const record = JSON.parse(await readFile(join(out, "s10-inputs.json"), "utf8")) as StageRecord;
    expect(record.dropScanThrough).toBe(40);
    // The scan read on from the pin, and the walk that gave the inputs stopped at it.
    expect(seen).toContain(`/api/v1/events/log?after=${THROUGH}&limit=1000`);
  });

  test("a neighbour the reserve holds is skipped under the read, with none of its fields written", async () => {
    const { text, inputs } = await requestAt(out, ASK_A);
    expect(inputs.candidates[HELD]).toEqual({ held: true, reserve: "held" });
    expect(text).not.toContain(`Neighbour question ${HELD}`);
    expect(text).not.toContain("held words");
    // Every ask hit was decided, and each before any request file was written.
    expect([...new Set(first.asked)].sort()).toEqual(
      [ASK_A, KEPT, LATER, HELD, UNSEEN, NEXT, PINNED].sort()
    );
    expect(first.filesWhenAsked.every((count) => count === 0)).toBe(true);
  });

  test("a request of one document is answered by the person's approval of it", async () => {
    const lines = await inventoryAt(out);
    expect(lines[1]?.judged).toEqual({ eventId: 24, type: "ask.opened", createdAt: at(24) });
    const { inputs } = await requestAt(out, ASK_B);
    expect(inputs.ask.question).toBe("Approve the settled spec?");
  });

  test("a payload that carries an answer at the judged moment makes the request a deviation", async () => {
    const lines = await inventoryAt(out);
    expect(lines[2]).toMatchObject({
      askId: ASK_C,
      state: "deviation",
      reason: "the payload at the judged moment carries an answer",
      requestSha256: null,
    });
    expect(existsSync(join(out, "requests", `${ASK_C}.json`))).toBe(false);
  });

  test("a second page is read only where the first held fifty results, and each page once", async () => {
    const offsets = seen.filter((path) => path.startsWith("/api/v1/search/results"));
    expect(offsets.filter((path) => path.includes("offset=50"))).toEqual([
      `/api/v1/search/results?q=${encodeURIComponent("ACME-3 title")}&limit=50&offset=50`,
    ]);
    // First title, Third words, ACME-3 title and Approve the settled spec?, the last at two pages.
    expect(offsets).toHaveLength(5);
    const { inputs } = await requestAt(out, ASK_B);
    expect(inputs.search.title.pages.map((page) => page.offset)).toEqual([0, 50]);
    expect(inputs.search.title.hits).toEqual([
      { id: KEPT, rank: 0.1, page: inputs.search.title.pages[1]?.sha256, position: 0 },
    ]);
  });

  test("each page is kept as its hits and the sha256 of its bytes, and never its body", async () => {
    const pages = await readdir(join(out, "pages"));
    expect(pages).toHaveLength(5);
    for (const name of pages) {
      const text = await readFile(join(out, "pages", name), "utf8");
      const page = JSON.parse(text);
      expect(name).toBe(`${page.sha256}.json`);
      expect(Object.keys(page).sort()).toEqual(["hits", "reads", "sha256"]);
      expect(text).not.toContain('"answer"');
    }
  });

  test("the stage record chains the build, the draw and every file written", async () => {
    const record = JSON.parse(await readFile(join(out, "s10-inputs.json"), "utf8")) as StageRecord;
    expect(record).toMatchObject({
      stage: "s10-inputs",
      state: "written",
      claimRun: START,
      reference: CLAIM,
      person: PERSON,
      throughEventId: THROUGH,
      build: { snapshotSha256 },
      draw: {
        drawSha256: sha256(await readFile(join(draw, "draw.json"), "utf8")),
        slicesSha256: sha256(await readFile(join(draw, "slices.json"), "utf8")),
      },
      reserveAsk: null,
      inventorySha256: sha256(await readFile(join(out, "inventory.jsonl"), "utf8")),
      dropScanThrough: 40,
    });
    expect(record.requests).toEqual({
      [ASK_A]: sha256((await requestAt(out, ASK_A)).text),
      [ASK_B]: sha256((await requestAt(out, ASK_B)).text),
    });
    expect(record.counts).toMatchObject({ requests: 3, ready: 2, deviations: 1, cases: 4 });
  });

  test("a resume takes each page archived, its first run's drop scan, and no request file again", async () => {
    const earlier = await requestAt(out, ASK_A);
    await rm(join(out, "requests", `${ASK_B}.json`));
    // An event of KEPT after the first run's scan: a resume scans through that run's last event
    // and no further, so KEPT stays kept.
    EVENTS.push(event(41, "comment.created", { ask_id: KEPT, body: "after the first scan" }));
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
    expect((await requestAt(out, ASK_A)).text).toBe(earlier.text);
    const { inputs } = await requestAt(out, ASK_B);
    expect(inputs.candidates[KEPT]).toMatchObject({ asOfEventId: 8 });
    const record = JSON.parse(await readFile(join(out, "s10-inputs.json"), "utf8")) as StageRecord;
    expect(record.dropScanThrough).toBe(40);
    // Only the request written again had its candidates decided again.
    expect(again.asked).toEqual([KEPT]);
  });

  test("refuses an --out holding another claim run's inputs, or files of no record", async () => {
    const other = join(await directory("other"), "inputs");
    await mkdir(other, { recursive: true });
    const record = JSON.parse(await readFile(join(out, "s10-inputs.json"), "utf8"));
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
      [{ claimRun: { id: "start-2", snapshotSha256 } }, "was drawn in the claim run start-2"],
      [{ claimRun: { id: START, snapshotSha256: "b".repeat(64) } }, "was drawn from the build"],
      [{ claimRun: null }, "was drawn in the claim run of no record"],
      [{ neverDecideApplied: true }, "does not record that it applied no never-decide rule"],
    ];
    for (const [overrides, words] of cases) {
      const other = await drawOf(snapshotSha256, overrides);
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
    version: 1,
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
    counts: { askHits: 0, candidates: 0, held: 0, reserveErrors: 0, droppedLaterEvent: 0, kept: 0 },
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
