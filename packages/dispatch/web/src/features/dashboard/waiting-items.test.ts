import { describe, expect, test } from "bun:test";

import type {
  Comment,
  Decision,
  IssueAskRow,
  IssueCoverage,
  IssueDecisions,
  MyRepliesResponse,
  MyReplyRow,
} from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import asksOpenBody from "./fixtures/asks-open.json";
import changesBody from "./fixtures/changes.json";
import coverageBody from "./fixtures/coverage.json";
import decisionsBody from "./fixtures/decisions.json";
import issueBody from "./fixtures/issue.json";
import myRepliesBody from "./fixtures/my-replies.json";
import {
  type NodeStateRead,
  requestedDocument,
  type WaitingIssue,
  type WaitingReads,
  waitingItems,
} from "./useWaitingItems";

// The reads of one Dashboard seed (`e2e/dashboard-seed.ts`'s `seedDashboard`), as the server at
// this branch's head answered them to the reader (`fixtures/README.md`): the issue, its open asks,
// its decisions, the decisions document's comments after one Change, its coverage and the
// reader's unread threads of the issue.
const READER = "alice";
const ISSUE: WaitingIssue = issueBody;
const ASKS: readonly IssueAskRow[] = asksOpenBody;
const DECISIONS: IssueDecisions = decisionsBody;
const CHANGES: readonly Comment[] = changesBody;
const COVERAGE: IssueCoverage = coverageBody;
const REPLIES: MyRepliesResponse = myRepliesBody;

function askOf(question: string): IssueAskRow {
  const found = ASKS.find((ask) => ask.question === question);
  if (found === undefined) throw new Error(`the captured asks hold no "${question}"`);
  return found;
}

function decisionOf(heading: string): Decision {
  const found = (DECISIONS.decisions ?? []).find((decision) => decision.heading === heading);
  if (found === undefined) throw new Error(`the captured decisions hold no "${heading}"`);
  return found;
}

function rowOf(subject: string): MyReplyRow {
  const found = (REPLIES.rows ?? []).find((row) => row.subject === subject);
  if (found === undefined) throw new Error(`the captured threads hold no "${subject}"`);
  return found;
}

function approvalRequest(): IssueAskRow {
  const found = ASKS.find((ask) => ask.kind === "approval");
  if (found === undefined) throw new Error("the captured asks hold no approval request");
  return found;
}

const BY_OPTION = askOf("Which order does the list take?");
const IN_WORDS = askOf("What does the line say when nothing waits?");
const REPLIED = askOf("Does a reply from here mark the thread read?");
const AGENTS_TURN = askOf("Which reads does the first load send?");
const HANDED_ON = askOf("Who reviews the tile words?");
const APPROVAL = approvalRequest();
const FIRST = decisionOf("Draw five items at a time");
const SECOND = decisionOf("Count what waits by its items");
const KEPT = decisionOf("Keep the list to what waits on the person");
const MESSAGE = rowOf("The tiles read the results and the coverage.");
const COMMENT = rowOf("Does the count drop as soon as I act?");
const DAEMON = rowOf("Planning started.");
const VERIFICATION_ID = ISSUE.verification_artifact_id;
const VERIFICATION = (ISSUE.artifacts ?? []).find((artifact) => artifact.id === VERIFICATION_ID);

/** The issue read with the Verification document's approval as `approval` gives it. */
function issueWithVerification(
  approval: NonNullable<NonNullable<WaitingIssue["artifacts"]>[number]["approval"]>
): WaitingIssue {
  return {
    ...ISSUE,
    artifacts: (ISSUE.artifacts ?? []).map((artifact) =>
      artifact.id === VERIFICATION_ID ? { ...artifact, approval } : artifact
    ),
  };
}

/** The captured approval request, covering the documents `documents` names. */
function requestOn(
  documents: NonNullable<NonNullable<IssueAskRow["approval"]>["documents"]>
): IssueAskRow {
  const approval = APPROVAL.approval;
  if (approval === undefined) throw new Error("the captured approval request names no document");
  const [first] = documents;
  return {
    ...APPROVAL,
    approval: {
      ...approval,
      artifact_id: first?.artifact_id ?? approval.artifact_id,
      documents,
      name: first?.name ?? approval.name,
    },
    artifact_id: first?.artifact_id ?? APPROVAL.artifact_id,
  };
}

const SPEC_DOCUMENTS = APPROVAL.approval?.documents ?? [];
/** The request of `include_verification`: the spec and the Verification document together. */
const BOTH_DOCUMENTS = requestOn([
  ...SPEC_DOCUMENTS,
  { artifact_id: VERIFICATION_ID, name: "verification.md", requested_version: 2, version: 2 },
]);
/** A request on a plan, which the seed's issue does not hold. */
const PLAN_ID = "4f1c2d3e-5a6b-4c7d-8e9f-0a1b2c3d4e5f";
const PLAN_REQUEST = requestOn([
  { artifact_id: PLAN_ID, name: "plan.md", requested_version: 1, version: 1 },
]);

/** The issue read with no approval on any document, so it adds no item. */
const ISSUE_WITHOUT_APPROVALS: WaitingIssue = {
  ...ISSUE,
  artifacts: (ISSUE.artifacts ?? []).map(({ approval: _approval, ...artifact }) => artifact),
};

/** The captured replies read holding `rows` alone, its `total` saying `total`. */
function repliesOf(rows: readonly MyReplyRow[], total = rows.length): MyRepliesResponse {
  return { ...REPLIES, rows: [...rows], total };
}

/** Reads in which nothing waits: each read answered, and holding nothing that waits. */
const NOTHING: WaitingReads = {
  asks: [],
  changes: [],
  coverage: COVERAGE,
  decisions: { ...DECISIONS, decisions: [] },
  issue: ISSUE_WITHOUT_APPROVALS,
  replies: repliesOf([]),
  viewer: READER,
};

/** Every read as captured: the seed's whole list. */
const CAPTURED: WaitingReads = {
  asks: ASKS,
  changes: [],
  coverage: COVERAGE,
  decisions: DECISIONS,
  issue: ISSUE,
  replies: REPLIES,
  viewer: READER,
};

/** The test id of each item, `<kind>-<id>`, as the browser rows read `dashboard-item-<kind>-<id>`,
 *  in the order the rule answers them. */
function drawn(reads: Partial<WaitingReads>, base: WaitingReads = NOTHING): readonly string[] {
  return waitingItems({ ...base, ...reads }).map((item) => `${item.kind}-${item.id}`);
}

describe("the item table", () => {
  test("Q1: an open question whose waiting_on_person is the reader is one question", () => {
    expect([BY_OPTION.waiting_on, BY_OPTION.waiting_on_person]).toEqual(["human", READER]);
    expect(drawn({ asks: [BY_OPTION] })).toEqual([`question-${BY_OPTION.id}`]);
  });

  test("Q2: an open question waiting on another person is none", () => {
    expect([HANDED_ON.waiting_on, HANDED_ON.waiting_on_person]).toEqual(["human", "bob"]);
    expect(drawn({ asks: [HANDED_ON] })).toEqual([]);
  });

  test("Q3: an open question waiting on an agent is none", () => {
    expect([AGENTS_TURN.waiting_on, AGENTS_TURN.waiting_on_person]).toEqual(["agent", READER]);
    expect(drawn({ asks: [AGENTS_TURN] })).toEqual([]);
  });

  test("D1: a decision in the review list is one decision", () => {
    expect(drawn({ decisions: { ...DECISIONS, decisions: [FIRST] } })).toEqual([
      `decision-${FIRST.block_id}`,
    ]);
  });

  test("D2: a kept decision is none", () => {
    expect([KEPT.kept?.kept_by, KEPT.waiting_review]).toEqual([READER, false]);
    expect(drawn({ decisions: { ...DECISIONS, decisions: [KEPT] } })).toEqual([]);
  });

  test("D3: a decision with a person's Change newer than its words, matched by the anchor of a comments?artifact= row, is none", () => {
    const second = { ...DECISIONS, decisions: [SECOND] };
    // The captured Change: the reader's comment anchored on the second decision's words.
    expect(CHANGES.map((comment) => [comment.author.id, comment.anchor?.quote])).toEqual([
      [READER, "Chosen so the count and the list agree."],
    ]);
    expect(drawn({ changes: CHANGES, decisions: second })).toEqual([]);
    // Without it, the decision waits for the reader's review.
    expect(drawn({ changes: [], decisions: second })).toEqual([`decision-${SECOND.block_id}`]);
  });

  test("D4: a decision whose every tie names no statement of the coverage read is none", () => {
    const tied = new Set((FIRST.links ?? []).map((link) => link.to.ref));
    const untied: IssueCoverage = {
      ...COVERAGE,
      requirements: (COVERAGE.requirements ?? []).filter(
        (requirement) => !tied.has(requirement.ref)
      ),
    };
    expect(untied.requirements?.length).toBeGreaterThan(0);
    expect(drawn({ coverage: untied, decisions: { ...DECISIONS, decisions: [FIRST] } })).toEqual(
      []
    );
  });

  test("A1: an approval request on the spec waiting on the reader is one item naming the spec and its version", () => {
    const items = waitingItems({ ...NOTHING, asks: [APPROVAL] });
    expect(items.map((item) => `${item.kind}-${item.id}`)).toEqual([`approval-${APPROVAL.id}`]);
    const [item] = items;
    expect(item?.kind === "approval" ? item.documents : []).toEqual([
      {
        artifact_id: ISSUE.primary_artifact_id,
        name: "spec.md",
        requested_version: 2,
        version: 2,
      },
    ]);
  });

  test("A2: one request covering the spec and the Verification document is one item naming both", () => {
    const items = waitingItems({ ...NOTHING, asks: [BOTH_DOCUMENTS] });
    expect(items.map((item) => `${item.kind}-${item.id}`)).toEqual([`approval-${APPROVAL.id}`]);
    const [item] = items;
    expect(
      (item?.kind === "approval" ? item.documents : []).map((document) => document.artifact_id)
    ).toEqual([ISSUE.primary_artifact_id, VERIFICATION_ID]);
  });

  test("A3: an approval request on the plan is one item", () => {
    const items = waitingItems({ ...NOTHING, asks: [PLAN_REQUEST] });
    expect(items.map((item) => `${item.kind}-${item.id}`)).toEqual([`approval-${APPROVAL.id}`]);
    const [item] = items;
    expect((item?.kind === "approval" ? item.documents : []).map((one) => one.name)).toEqual([
      "plan.md",
    ]);
  });

  test("S1: the Verification document's approval reads stale, by the reader: one stale item", () => {
    expect([VERIFICATION?.approval?.state, VERIFICATION?.approval?.by?.id]).toEqual([
      "stale",
      READER,
    ]);
    expect(drawn({ issue: ISSUE })).toEqual([`stale-${VERIFICATION_ID}`]);
  });

  test("S2: it reads stale, by another person: none", () => {
    const approval = VERIFICATION?.approval;
    if (approval === undefined)
      throw new Error("the captured Verification document has no approval");
    const byBob = issueWithVerification({ ...approval, by: { id: "bob", kind: "user" } });
    expect(drawn({ issue: byBob })).toEqual([]);
  });

  test("S3: it reads stale and an open request covers the document: the request's item alone", () => {
    expect(drawn({ asks: [BOTH_DOCUMENTS], issue: ISSUE })).toEqual([`approval-${APPROVAL.id}`]);
  });

  test("S4: it reads approved after the server carried it: none", () => {
    const approval = VERIFICATION?.approval;
    if (approval === undefined)
      throw new Error("the captured Verification document has no approval");
    const carried = issueWithVerification({
      ...approval,
      state: "approved",
      version: approval.latest_version,
    });
    expect(drawn({ issue: carried })).toEqual([]);
  });

  test("T1: an unread me/replies row whose root_author is a person is one thread item", () => {
    expect(COMMENT.root_author).toMatchObject({ id: READER, kind: "user" });
    expect(drawn({ replies: repliesOf([COMMENT]) })).toEqual([`thread-${COMMENT.thread_id}`]);
    // An agent's root is no daemon's either.
    expect(drawn({ replies: repliesOf([MESSAGE]) })).toEqual([`thread-${MESSAGE.thread_id}`]);
  });

  test("T2: a row whose root_author is the Legion daemon's session of the issue's project is none", () => {
    expect(DAEMON.root_author).toEqual({ id: `legion-daemon:${ISSUE.project}`, kind: "session" });
    expect(drawn({ replies: repliesOf([DAEMON]) })).toEqual([]);
  });

  test("O1: an open ask whose thread the reader wrote in and an agent wrote last is one item, in the questions part", () => {
    expect(REPLIED.last_reply?.author.id).not.toBe(READER);
    // `me/replies` leaves such an ask out (`myRepliesOwner`); a row of it is drawn once all the
    // same, as the question.
    const askRow: MyReplyRow = {
      ...COMMENT,
      kind: "ask",
      ref: `/issues/${ISSUE.key}/asks/${REPLIED.id}`,
      root_author: REPLIED.author,
      subject: REPLIED.question,
      thread_id: REPLIED.id,
    };
    expect(drawn({ asks: [REPLIED], replies: repliesOf([askRow]) })).toEqual([
      `question-${REPLIED.id}`,
    ]);
  });

  test("V1: an open ask whose node in ?view=decisions reads overtaken is none", () => {
    const node = (state: string): NodeStateRead => ({ id: BY_OPTION.id, state });
    expect(drawn({ asks: [BY_OPTION], nodes: [node("overtaken")] })).toEqual([]);
    expect(drawn({ asks: [BY_OPTION], nodes: [node("live")] })).toEqual([
      `question-${BY_OPTION.id}`,
    ]);
  });

  test("V2: a decision to review whose node reads overtaken is none", () => {
    const node = (state: string): NodeStateRead => ({
      block_id: FIRST.block_id,
      id: "7d0e3c1a-2b4f-4e6d-9a8b-1c2d3e4f5a6b",
      state,
    });
    const first = { ...DECISIONS, decisions: [FIRST] };
    expect(drawn({ decisions: first, nodes: [node("overtaken")] })).toEqual([]);
    expect(drawn({ decisions: first, nodes: [node("live")] })).toEqual([
      `decision-${FIRST.block_id}`,
    ]);
  });

  test("N1: items of each kind at different times stand newest first", () => {
    const items = waitingItems(CAPTURED);
    // The seed wrote each of its items after every write of the one before it (`items`, newest
    // first), so its order is the list's.
    expect(items.map((item) => `${item.kind}-${item.id}`)).toEqual([
      `thread-${MESSAGE.thread_id}`,
      `thread-${COMMENT.thread_id}`,
      `stale-${VERIFICATION_ID}`,
      `approval-${APPROVAL.id}`,
      `decision-${SECOND.block_id}`,
      `decision-${FIRST.block_id}`,
      `question-${REPLIED.id}`,
      `question-${IN_WORDS.id}`,
      `question-${BY_OPTION.id}`,
    ]);
    for (const [index, item] of items.entries()) {
      const next = items[index + 1];
      if (next !== undefined) expect(compareTimestamps(item.at, next.at)).toBeGreaterThan(0);
    }
  });

  test("C1: seven items and reads whose total says nine count seven", () => {
    const reads: WaitingReads = {
      ...CAPTURED,
      asks: ASKS.filter((ask) => ask.id !== APPROVAL.id),
      replies: repliesOf([COMMENT, DAEMON], 9),
    };
    expect(waitingItems(reads).length).toBe(7);
  });
});

/** The captured approval request, its ask on `artifactId` and its approval naming `slug`. */
function requestNaming(artifactId: string | null, slug: string): IssueAskRow {
  const named = APPROVAL.approval_artifact;
  if (named === undefined) throw new Error("the captured approval request names no slug");
  return { ...APPROVAL, approval_artifact: { ...named, slug }, artifact_id: artifactId };
}

// The document an approval request is on, which the header's approval control for it is drawn on
// (`requestedDocument`), as `main`'s count read it (`waitingOnReader`'s `approvals`): the ask's
// own document, else the one its approval names by slug, else the spec.
describe("the document an approval request is on", () => {
  test("a request with no artifact_id and no slug the issue holds is on the spec", () => {
    expect(APPROVAL.artifact_id).toBeNull();
    expect(requestedDocument(requestNaming(null, "plan-md"), ISSUE)).toBe(
      ISSUE.primary_artifact_id
    );
    const { approval_artifact: _named, ...unnamed } = APPROVAL;
    expect(requestedDocument(unnamed, ISSUE)).toBe(ISSUE.primary_artifact_id);
  });

  test("a request whose ask names an artifact_id is on that document, whatever slug it names", () => {
    expect(requestedDocument(requestNaming(VERIFICATION_ID, "spec"), ISSUE)).toBe(VERIFICATION_ID);
  });

  test("a request with no artifact_id is on the issue's document its approval names by slug", () => {
    const slug = VERIFICATION?.slug;
    if (slug === undefined) throw new Error("the captured issue holds no Verification document");
    expect(requestedDocument(requestNaming(null, slug), ISSUE)).toBe(VERIFICATION_ID);
  });
});

test("nothing waits on a reader whose login has not resolved", () => {
  expect(drawn({ viewer: undefined }, CAPTURED)).toEqual([]);
});

test("no decision is drawn until the Changes read answers, so none drawn leaves on its answer", () => {
  expect(drawn({ changes: undefined, decisions: { ...DECISIONS, decisions: [SECOND] } })).toEqual(
    []
  );
});
