import type { LegionBoardCard, LegionBoardResponse } from "../../api/types";

const card = (
  key: string,
  phase: string,
  overrides: Partial<LegionBoardCard> = {}
): LegionBoardCard => ({
  key,
  title: key,
  priority: null,
  phase,
  phase_since: "2026-11-10T00:00:00Z",
  slot: null,
  tree_slot: null,
  slot_since: null,
  waiting_place: null,
  roles: [],
  checks_verdict: null,
  blocked_by: [],
  waiting_on: null,
  slot_use: null,
  waiting: null,
  tree: key,
  children: 0,
  ...overrides,
});

export function boardWithFacts(overrides: Partial<LegionBoardResponse> = {}): LegionBoardResponse {
  return {
    project: "ACME",
    header: {
      slots_in_use: 2,
      slot_cap: 4,
      trees: 2,
      queue_length: 3,
      read_at: "2026-11-11T01:00:00Z",
      read_ok: true,
      read_error: null,
    },
    columns: [
      {
        id: "queue",
        label: "Queue",
        cards: [
          card("ACME-3", "admitted", {
            waiting_place: 3,
            waiting: { reason: "ready", since: "2026-11-10T00:00:00Z", blocked_by: [] },
          }),
          card("ACME-4", "admitted", {
            waiting_place: 1,
            waiting: { reason: "slot", since: "2026-11-10T01:00:00Z", blocked_by: [] },
          }),
          card("ACME-5", "admitted", {
            waiting_place: 2,
            waiting: { reason: "ready", since: "2026-11-10T02:00:00Z", blocked_by: [] },
          }),
        ],
      },
      { id: "design", label: "Design", cards: [] },
      { id: "planning", label: "Planning", cards: [] },
      {
        id: "implementing",
        label: "Implementing",
        cards: [
          card("ACME-1", "implementing", {
            slot: 0,
            slot_since: "2026-11-10T00:00:00Z",
            slot_use: "working",
            roles: ["implementer"],
          }),
        ],
      },
      { id: "testing", label: "Testing", cards: [] },
      {
        id: "reviewing",
        label: "Reviewing",
        cards: [
          card("ACME-2", "reviewing", {
            slot: 1,
            slot_since: "2026-11-10T00:00:00Z",
            slot_use: "held_children",
            tree: "ACME-1",
            tree_slot: 0,
          }),
        ],
      },
      { id: "accepting", label: "Accepting", cards: [] },
      { id: "merging", label: "Merging", cards: [] },
      {
        id: "awaiting_person",
        label: "Waiting on a person",
        cards: [
          card("ACME-7", "awaiting_person", {
            waiting: { reason: "person", since: "2026-11-10T03:00:00Z", blocked_by: [] },
          }),
        ],
      },
      {
        id: "held",
        label: "Held",
        cards: [
          card("ACME-6", "held", {
            waiting: {
              reason: "blocked",
              since: "2026-11-10T04:00:00Z",
              blocked_by: ["ACME-1"],
            },
          }),
        ],
      },
    ],
    issues: {
      "ACME-1": {
        phase_hours: { planning: 2, implementing: 6 },
        work_hours: 8,
        review_rounds: 1,
        root: true,
        tree: "ACME-1",
      },
      "ACME-2": {
        phase_hours: { reviewing: 1 },
        work_hours: 1,
        review_rounds: 1,
        root: false,
        tree: "ACME-1",
      },
      "ACME-3": {
        phase_hours: {},
        work_hours: 0,
        review_rounds: 0,
        root: true,
        tree: "ACME-3",
      },
      "ACME-4": {
        phase_hours: { planning: 1 },
        work_hours: 1,
        review_rounds: 0,
        root: true,
        tree: "ACME-4",
      },
      "ACME-5": {
        phase_hours: { implementing: 1 },
        work_hours: 1,
        review_rounds: 0,
        root: true,
        tree: "ACME-5",
      },
      "ACME-7": {
        phase_hours: { awaiting_person: 2 },
        work_hours: 2,
        review_rounds: 0,
        root: true,
        tree: "ACME-7",
      },
    },
    settings: {},
    setting_sources: {},
    computed_at: "2026-11-11T01:00:00Z",
    ...overrides,
  };
}
