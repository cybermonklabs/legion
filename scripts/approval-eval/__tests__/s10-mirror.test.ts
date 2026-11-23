import { describe, expect, test } from "bun:test";
import { microsecondsOf } from "../reserve";
import {
  askDocsOf,
  type JudgedForNeighbours,
  NEIGHBOURS_KEPT,
  type NeighbourCandidate,
  neighbourFault,
  neighbourOrder,
} from "../s10-mirror";

/**
 * S10's own reads of a request, mirrored from its Python code in `s10-mirror.ts`, over values made
 * here: the documents an approval names, as `ask_docs` reads them, and the candidates its
 * `neighbours` reads, in its order and up to its eighth neighbour.
 */

const PERSON = "person@example.test";

/** The judged request: its first opening is after every neighbour's answer here. */
const JUDGED: JudgedForNeighbours = {
  person: PERSON,
  issueKey: "ACME-1",
  openedAt: microsecondsOf("2026-11-17T10:16:00.000001Z") ?? 0,
  documents: new Set(["doc-a"]),
};

/** A candidate with its fields, which is the one kind `neighbourFault` reads anything of. */
type Kept = Extract<NeighbourCandidate, { ask: unknown }>;

/** A candidate S10's neighbours keeps: every field it reads is there, and none stops it. */
function kept(index: number): Kept {
  return {
    ask: {
      state: "answered",
      kind: "approval",
      issue_key: "ACME-2",
      question: `Neighbour question ${index}`,
      approval: { artifact_id: `doc-n${index}`, name: "spec.md", version: 1 },
      answer: { user: PERSON, at: "2026-11-17T10:00:00Z", selected: ["Approve"], text: null },
    },
    issue: { key: "ACME-2", title: "Neighbour issue" },
  };
}

/** A kept candidate with `fields` in place of its ask's own. */
function keptWith(index: number, fields: Record<string, unknown>): Kept {
  const candidate = kept(index);
  return { ...candidate, ask: { ...candidate.ask, ...fields } };
}

/** An answer that names no time, which S10 reads once a candidate passes its earlier reads. */
const UNTIMED = { answer: { user: PERSON, selected: ["Approve"], text: null } };

/** Throws, for any read of the value it guards. */
function unread(): never {
  throw new Error("a candidate past S10's eighth neighbour was read");
}

/** An ask every read of which throws: a candidate `neighbourFault` must never reach. */
const UNREAD_HANDLER: ProxyHandler<object> = {
  get: unread,
  has: unread,
  ownKeys: unread,
  getOwnPropertyDescriptor: unread,
};
const UNREAD: Record<string, unknown> = new Proxy({}, UNREAD_HANDLER);

/** The ask ids `ask-1` to `ask-<count>`, in that order. */
function asks(count: number): string[] {
  return Array.from({ length: count }, (_, index) => `ask-${index + 1}`);
}

/** Each of `order` kept, by its place in it from 1. */
function allKept(order: readonly string[]): Record<string, NeighbourCandidate> {
  return Object.fromEntries(order.map((askId, index) => [askId, kept(index + 1)]));
}

describe("S10's neighbours, as neighbourFault reads them", () => {
  test("keeps eight and reads no candidate after the eighth, so a fault of the ninth is none", () => {
    expect(NEIGHBOURS_KEPT).toBe(8);
    const order = asks(9);
    const candidates = { ...allKept(order), "ask-9": { ask: UNREAD, issue: null } };
    expect(neighbourFault(order, candidates, JUDGED)).toBeNull();
    // A read of the ninth would have thrown.
    expect(() => neighbourFault(["ask-9"], candidates, JUDGED)).toThrow("past S10's eighth");
  });

  test("a fault of the eighth neighbour is the request's, and the ninth is still never read", () => {
    const order = asks(9);
    const candidates = {
      ...allKept(order),
      "ask-8": keptWith(8, UNTIMED),
      "ask-9": { ask: UNREAD, issue: null },
    };
    expect(neighbourFault(order, candidates, JUDGED)).toEqual({
      askId: "ask-8",
      field: "answer.at",
    });
  });

  test("a held or dropped candidate is passed over, and counts toward no eight", () => {
    const order = ["held", "dropped", ...asks(8)];
    const candidates: Record<string, NeighbourCandidate> = {
      ...allKept(asks(7)),
      held: { held: true },
      dropped: { dropped: "later-event" },
      "ask-8": keptWith(8, UNTIMED),
    };
    expect(neighbourFault(order, candidates, JUDGED)).toEqual({
      askId: "ask-8",
      field: "answer.at",
    });
  });

  test("a candidate whose documents list holds an entry that is no document faults on its path", () => {
    const lead = { artifact_id: "doc-b", name: "spec.md", version: 2 };
    const candidate = keptWith(1, { approval: { ...lead, documents: [lead, null] } });
    expect(neighbourFault(["ask-1"], { "ask-1": candidate }, JUDGED)).toEqual({
      askId: "ask-1",
      field: "approval.documents[1]",
    });
  });

  test("takes each ask once at its best rank, the hit met first ahead where two ranks are equal", () => {
    const hits = [
      { id: "title-low", rank: 0.5 },
      { id: "both", rank: 0.6 },
      { id: "title-tie", rank: 0.8 },
      { id: "no-rank", rank: null },
      { id: "question-tie", rank: 0.8 },
      { id: "both", rank: 0.9 },
    ];
    expect(neighbourOrder(hits)).toEqual([
      "both",
      "title-tie",
      "question-tie",
      "title-low",
      "no-rank",
    ]);
  });
});

describe("the documents an approval names, as S10's ask_docs reads them", () => {
  const lead = { artifact_id: "doc-a", name: "spec.md", version: 2 };

  test("reads every entry of a list it takes for true, and the lead document only where none", () => {
    expect(askDocsOf({ ...lead, documents: [null] })).toEqual({ missing: "approval.documents[0]" });
    const malformed = [
      { artifact_id: "doc-b", name: "b.md", version: 1 },
      { artifact_id: "doc-c", version: 1 },
    ];
    expect(askDocsOf({ ...lead, documents: malformed })).toEqual({
      missing: "approval.documents[1].name",
    });
    expect(askDocsOf({ ...lead, documents: "doc-b" })).toEqual({ missing: "approval.documents" });
    expect(askDocsOf({ ...lead, documents: [] })).toEqual({ documents: [lead] });
    expect(askDocsOf(lead)).toEqual({ documents: [lead] });
    expect(askDocsOf(null)).toEqual({ documents: [] });
  });
});
