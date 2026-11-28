import { describe, expect, test } from "bun:test";

import type { LegionIssueResponse, LegionIssueState, LegionIssueWaitingOn } from "../../api/types";
import {
  type LegionBlock,
  type LegionDocuments,
  type LegionPlacePart,
  legionBlockOf,
  waitReadsDocuments,
} from "./legion-block";

// The Legion block of Details (dispatch://INFRA-4870/spec#b-r-block), read as the plan's table A
// reads the parent's "Words for the state" (dispatch://INFRA-1336/spec): what the block draws of
// each stored state, from the issue's Legion read and its documents. The words themselves are
// `legion-words.test.ts`'s; how they are drawn is `LegionBlock.test.tsx`'s.

/** The page's own issue, whose key every link of the block names. */
const KEY = "ACME-1";

/** An ask of the issue, as the daemon names the oldest question still with a person. */
const ASK_ID = "0193a6f2-4c1d-7b3e-9a10-5d2c8e6f7a41";
/** A document of the issue, and the slug its list gives it. */
const DOCUMENT_ID = "0193a6f2-9e0b-7c55-8d21-3f4a5b6c7d82";
const DOCUMENT_SLUG = "design-md";
/** The issue's documents as `GET /api/v1/issues/{key}/artifacts` lists them. */
const DOCUMENTS: LegionDocuments = [
  { id: "0193a6f2-1111-7000-8000-000000000001", slug: "spec" },
  { id: DOCUMENT_ID, slug: DOCUMENT_SLUG },
];

const PHASE_SINCE = "2026-11-20T18:35:00Z";
const READ_AT = "2026-11-20T20:22:00Z";

/** A1's state: `implementing`, a slot, the architect and the implementer in a turn, the tester
 *  idle and the planner paused, in no line, held by nothing and waiting on nobody. */
const IMPLEMENTING: LegionIssueState = {
  blocked_by: [],
  phase: "implementing",
  phase_since: PHASE_SINCE,
  phase_start_seen: true,
  roles: [
    { role: "architect", state: "working" },
    { role: "implementer", state: "working" },
    { role: "tester", state: "idle" },
    { role: "planner", state: "suspended" },
  ],
  slot: 0,
  waiting_line: null,
  waiting_on: null,
};

/** The issue's Legion read: A1's state of a project that names a daemon, and a good read. */
function answerOf(
  state: Partial<LegionIssueState> | null,
  over: Partial<LegionIssueResponse> = {}
): LegionIssueResponse {
  return {
    configured: true,
    issue: KEY,
    project: "ACME",
    read: { at: READ_AT, error: null, ok: true },
    runs_revision: 3,
    state: state === null ? null : { ...IMPLEMENTING, ...state },
    ...over,
  };
}

/** What the block draws of `state` over A1's, with the issue's documents read. */
function blockOf(
  state: Partial<LegionIssueState> | null,
  over: Partial<LegionIssueResponse> = {}
): LegionBlock | null {
  return legionBlockOf(answerOf(state, over), KEY, DOCUMENTS);
}

/** What the block draws of its place and its wait, for `state` over A1's. */
function placeOf(state: Partial<LegionIssueState>): LegionPlacePart[] {
  const block = blockOf(state);
  if (block === null) throw new Error("the state drew no block");
  return [...block.place];
}

/** `awaiting_person` with the wait `waiting_on`, out of every slot and line. */
function waitingOn(waiting_on: LegionIssueWaitingOn | null): Partial<LegionIssueState> {
  return { phase: "awaiting_person", slot: null, waiting_on };
}

const AT_WORK: LegionPlacePart = { kind: "at_work" };

describe("the block reads each state", () => {
  test("A1: implementing, a slot, two roles in a turn, one idle, one paused, a good read", () => {
    expect(blockOf({})).toEqual({
      phase: "Implementing",
      place: [AT_WORK],
      read: { at: READ_AT, failed: false },
      roles: [
        { name: "architect", own: false, state: "in a turn" },
        { name: "implementer", own: false, state: "in a turn" },
        { name: "tester", own: false, state: "idle" },
        { name: "planner", own: false, state: "paused" },
      ],
      since: { at: PHASE_SINCE, before: false },
    });
  });

  test("A2: no slot, third of five in the line", () => {
    expect(placeOf({ slot: null, waiting_line: { length: 5, place: 3 } })).toEqual([
      { ending: "rd", kind: "line", length: 5, place: 3 },
    ]);
  });

  test("A3: a slot, and second of four in the line", () => {
    expect(placeOf({ slot: 2, waiting_line: { length: 4, place: 2 } })).toEqual([
      AT_WORK,
      { ending: "nd", kind: "line", length: 4, place: 2 },
    ]);
  });

  test("A4: blocked_by names two issues", () => {
    expect(placeOf({ blocked_by: ["ACME-7", "ACME-9"], slot: null })).toEqual([
      { keys: ["ACME-7", "ACME-9"], kind: "waits_for" },
    ]);
    expect(
      placeOf({ blocked_by: ["ACME-7", "ACME-9"], waiting_line: { length: 4, place: 1 } })
    ).toEqual([
      AT_WORK,
      { ending: "st", kind: "line", length: 4, place: 1 },
      { keys: ["ACME-7", "ACME-9"], kind: "waits_for" },
    ]);
  });

  test("A5: awaiting_person on an ask whose id is a UUID", () => {
    const state = waitingOn({ ask: ASK_ID, kind: "ask", since: PHASE_SINCE });
    expect(placeOf(state)).toEqual([
      { href: `/issues/${KEY}/asks/${ASK_ID}`, kind: "waits_on_person", members: ["ask", ASK_ID] },
    ]);
    expect(waitReadsDocuments(answerOf(state)), "an ask's wait read the documents").toBe(false);
  });

  test("A6: the same, with HTML markup in ask and kind", () => {
    const kind = '<a href="https://evil.example/">gate</a>';
    const ask = `<img src=x onerror="alert(1)">${ASK_ID}`;
    expect(placeOf(waitingOn({ ask, kind }))).toEqual([
      { href: null, kind: "waits_on_person", members: [kind, ask] },
    ]);
  });

  test("A7: the same, with the ask ../../settings", () => {
    expect(placeOf(waitingOn({ ask: "../../settings", kind: "ask" }))).toEqual([
      { href: null, kind: "waits_on_person", members: ["ask", "../../settings"] },
    ]);
  });

  test("A8: a gate wait on a document of the issue at version 7", () => {
    const state = waitingOn({ artifact_id: DOCUMENT_ID, gate_version: 7, kind: "gate" });
    expect(placeOf(state)).toEqual([
      {
        href: `/issues/${KEY}/artifacts/${DOCUMENT_SLUG}?v=7`,
        kind: "waits_on_person",
        members: ["gate", DOCUMENT_ID, "7"],
      },
    ]);
    expect(waitReadsDocuments(answerOf(state))).toBe(true);
  });

  test("A9: the same, gate_version -1", () => {
    for (const gate_version of [-1, 0, 1.5]) {
      const state = waitingOn({ artifact_id: DOCUMENT_ID, gate_version, kind: "gate" });
      expect(placeOf(state), String(gate_version)).toEqual([
        {
          href: null,
          kind: "waits_on_person",
          members: ["gate", DOCUMENT_ID, String(gate_version)],
        },
      ]);
      expect(
        waitReadsDocuments(answerOf(state)),
        `a gate_version of ${gate_version} read the documents`
      ).toBe(false);
    }
  });

  test("A10: a gate wait whose artifact_id no document of the list holds, or the documents not read", () => {
    const elsewhere = "0193a6f2-2222-7000-8000-000000000009";
    const unlisted = waitingOn({ artifact_id: elsewhere, gate_version: 7, kind: "gate" });
    expect(placeOf(unlisted)).toEqual([
      { href: null, kind: "waits_on_person", members: ["gate", elsewhere, "7"] },
    ]);
    const unread = waitingOn({ artifact_id: DOCUMENT_ID, gate_version: 7, kind: "gate" });
    expect(legionBlockOf(answerOf(unread), KEY, undefined)?.place).toEqual([
      { href: null, kind: "waits_on_person", members: ["gate", DOCUMENT_ID, "7"] },
    ]);
    // An artifact_id that is no UUID is never looked up, so its gate reads no documents.
    const notAnId = waitingOn({ artifact_id: DOCUMENT_SLUG, gate_version: 7, kind: "gate" });
    expect(placeOf(notAnId)).toEqual([
      { href: null, kind: "waits_on_person", members: ["gate", DOCUMENT_SLUG, "7"] },
    ]);
    expect(waitReadsDocuments(answerOf(notAnId))).toBe(false);
  });

  test("A11: a wait of another kind", () => {
    // A member of another kind is never a target, though it holds a UUID of an ask or a document.
    const state = waitingOn({
      artifact_id: DOCUMENT_ID,
      ask: ASK_ID,
      gate_version: 7,
      kind: "review",
    });
    expect(placeOf(state)).toEqual([
      {
        href: null,
        kind: "waits_on_person",
        members: ["review", ASK_ID, DOCUMENT_ID, "7"],
      },
    ]);
    expect(waitReadsDocuments(answerOf(state))).toBe(false);
  });

  test("A12: awaiting_person, waiting on nothing the read names", () => {
    expect(placeOf(waitingOn(null))).toEqual([
      { href: null, kind: "waits_on_person", members: [] },
    ]);
  });

  test("A13: awaiting_person, the wait answered, a slot, fourth of six in the line", () => {
    const state: Partial<LegionIssueState> = {
      phase: "awaiting_person",
      slot: 1,
      waiting_line: { length: 6, place: 4 },
      waiting_on: {
        answered_at: READ_AT,
        answered_by: "ask",
        ask: ASK_ID,
        kind: "ask",
        since: PHASE_SINCE,
      },
    };
    expect(placeOf(state)).toEqual([{ ending: "th", kind: "line", length: 6, place: 4 }]);
    expect(waitReadsDocuments(answerOf(state))).toBe(false);
    // A gate wait answered reads no documents: it is drawn as its place alone.
    const gate = {
      ...state,
      waiting_on: { answered_at: READ_AT, artifact_id: DOCUMENT_ID, gate_version: 7, kind: "gate" },
    };
    expect(waitReadsDocuments(answerOf(gate))).toBe(false);
  });

  test("A14: accepting, retro, merging and awaiting_merge are past review", () => {
    const phases = {
      accepting: "Accepting",
      awaiting_merge: "Merging",
      merging: "Merging",
      retro: "Retro",
    };
    for (const [phase, word] of Object.entries(phases)) {
      const block = blockOf({ phase, slot: null });
      expect(block?.phase, phase).toBe(word);
      expect(block?.place, phase).toEqual([{ kind: "past_review" }]);
    }
  });

  test("A15: admitted, no slot, no line, no blocker", () => {
    expect(blockOf({ phase: "admitted", slot: null })?.place).toEqual([{ kind: "not_started" }]);
  });

  test("A16: admitted, in the line", () => {
    expect(
      placeOf({ phase: "admitted", slot: null, waiting_line: { length: 12, place: 11 } })
    ).toEqual([{ ending: "th", kind: "line", length: 12, place: 11 }]);
    expect(placeOf({ blocked_by: ["ACME-7"], phase: "admitted", slot: null })).toEqual([
      { keys: ["ACME-7"], kind: "waits_for" },
    ]);
  });

  test("A17: held, with a slot and without", () => {
    for (const slot of [0, null]) {
      const block = blockOf({ phase: "held", slot, waiting_line: { length: 3, place: 2 } });
      expect(block?.phase, `slot ${slot}`).toBe("Held");
      expect(block?.place, `slot ${slot}`).toEqual([]);
    }
  });

  test("A18: planning, no slot, no line", () => {
    expect(placeOf({ phase: "planning", slot: null })).toEqual([]);
  });

  test("A19: every role idle; one retired; one of a state the page does not know; no role", () => {
    const idle = blockOf({
      roles: [
        { role: "architect", state: "idle" },
        { role: "planner", state: "ready" },
        { role: "tester", state: "registered" },
      ],
    });
    expect(idle?.roles).toEqual([
      { name: "architect", own: false, state: "idle" },
      { name: "planner", own: false, state: "idle" },
      { name: "tester", own: false, state: "idle" },
    ]);
    const mixed = blockOf({
      roles: [
        { role: "architect", state: "working" },
        { role: "reviewer", state: "retired" },
        { role: "merger", state: "hibernating" },
        { role: "<b>scout</b>", state: "queued" },
      ],
    });
    expect(mixed?.roles).toEqual([
      { name: "architect", own: false, state: "in a turn" },
      { name: "merger", own: false, state: "in another state" },
      { name: "<b>scout</b>", own: true, state: "starting" },
    ]);
    expect(blockOf({ roles: [] })?.roles).toEqual([]);
    expect(blockOf({ roles: null })?.roles).toEqual([]);
  });

  test("A20: Dispatch did not see the phase start", () => {
    expect(blockOf({ phase_start_seen: false })?.since).toEqual({ at: PHASE_SINCE, before: true });
  });

  test("A21: the newest read failed, with an error text", () => {
    const block = blockOf(
      {},
      { read: { at: READ_AT, error: "Get http://daemon/legion/v1/state: refused", ok: false } }
    );
    expect(block?.read).toEqual({ at: READ_AT, failed: true });
    expect(JSON.stringify(block), "the block holds the read's error text").not.toContain("refused");
    // The stored state stays.
    expect(block?.phase).toBe("Implementing");
    expect(block?.place).toEqual([AT_WORK]);
  });

  test("A22: no read recorded", () => {
    expect(blockOf({}, { read: null })?.read).toBeNull();
  });

  test("A23: done; unrecorded; state null; configured false", () => {
    expect(blockOf({ phase: "done" })).toBeNull();
    expect(blockOf({ phase: "unrecorded" })).toBeNull();
    expect(blockOf(null)).toBeNull();
    expect(blockOf(null, { configured: false, read: null })).toBeNull();
  });

  test("A24: a phase the page does not know; integrating; awaiting_children", () => {
    expect(blockOf({ phase: "awaiting_review" })?.phase).toBe("In another phase");
    expect(blockOf({ phase: "integrating" })?.phase).toBe("Integrating");
    expect(blockOf({ phase: "awaiting_children", slot: null })).toMatchObject({
      phase: "Waiting on children",
      place: [],
    });
  });
});
