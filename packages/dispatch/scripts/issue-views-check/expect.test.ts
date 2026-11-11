import { expect, test } from "bun:test";

import { expectedStageCount, expectedViewCounts } from "./expect";

test("computes each built-in view from independent API facts", () => {
  const counts = expectedViewCounts({
    openIssues: [
      { key: "OPEN-TRIAGE", status: "triage", created_by: { id: "alice" } },
      { key: "OPEN-IMPLEMENTING", status: "in_progress", created_by: { id: "bob" } },
      { key: "OPEN-QUEUE", status: "todo", created_by: { id: "alice" } },
      { key: "OPEN-REVIEW", status: "needs_review", created_by: { id: "bob" } },
      { key: "OPEN-BACKLOG", status: "backlog", created_by: { id: "bob" } },
    ],
    doneIssues: [
      { key: "DONE-WORKED-ONE", status: "done", closed_at: "2026-10-24T12:00:00Z" },
      { key: "DONE-WORKED-TWO", status: "done", closed_at: "2026-10-30T12:00:00Z" },
      { key: "DONE-NO-FACTS", status: "done", closed_at: "2026-10-30T12:00:00Z" },
    ],
    board: {
      columns: [
        {
          id: "implementing",
          cards: [{ key: "OPEN-IMPLEMENTING", slot_use: "working", waiting: null }],
        },
        {
          id: "queue",
          cards: [
            {
              key: "OPEN-QUEUE",
              slot_use: "working",
              waiting: { reason: "ready", since: "2026-11-10T12:00:00Z", blocked_by: [] },
            },
          ],
        },
        {
          id: "reviewing",
          cards: [
            {
              key: "DONE-NO-FACTS",
              slot_use: "held_idle",
              waiting: { reason: "blocked", since: null, blocked_by: [] },
            },
          ],
        },
      ],
      issues: {
        "DONE-WORKED-ONE": {
          phase_hours: { implementing: 2 },
          work_hours: 2,
          review_rounds: 1,
          root: true,
          tree: "DONE-WORKED-ONE",
        },
        "DONE-WORKED-TWO": {
          phase_hours: {},
          work_hours: 0,
          review_rounds: 0,
          root: false,
          tree: "DONE-WORKED-ONE",
        },
      },
    },
    hasLegion: true,
    login: "alice",
    nowMs: Date.parse("2026-11-11T12:00:00Z"),
  });

  expect(counts).toEqual({
    open: 5,
    triage: 1,
    waiting: 1,
    "in-flight": 2,
    slots: 2,
    finished: 2,
    mine: 2,
  });
});

test("counts every closed issue in Finished without a Legion daemon", () => {
  const counts = expectedViewCounts({
    openIssues: [],
    doneIssues: [{ key: "DONE", status: "done", closed_at: "2026-11-10T12:00:00Z" }],
    board: { columns: [] },
    hasLegion: false,
    login: "alice",
    nowMs: Date.parse("2026-11-11T12:00:00Z"),
  });

  expect(counts.finished).toBe(1);
});

test("maps a Legion-labelled in-progress issue to its lifecycle phase", () => {
  const count = expectedStageCount(
    {
      openIssues: [
        { key: "LEGION", status: "in_progress", labels: ["Legion"] },
        { key: "PLAIN", status: "in_progress" },
      ],
      doneIssues: [],
      board: { columns: [] },
      hasLegion: true,
      login: "alice",
      nowMs: Date.parse("2026-11-11T12:00:00Z"),
    },
    "phase:implementing"
  );

  expect(count).toBe(1);
});

test("uses the inclusive 28-day closed window", () => {
  const counts = expectedViewCounts({
    openIssues: [],
    doneIssues: [
      { key: "BOUNDARY", status: "done", closed_at: "2026-10-14T12:00:00Z" },
      { key: "OLD", status: "done", closed_at: "2026-10-13T12:00:00Z" },
      { key: "MISSING", status: "done", closed_at: null },
      { key: "FUTURE", status: "done", closed_at: "2026-11-12T12:00:00Z" },
    ],
    board: {
      columns: [],
      issues: {
        BOUNDARY: {},
        OLD: {},
        MISSING: {},
        FUTURE: {},
      },
    },
    hasLegion: true,
    login: "alice",
    nowMs: Date.parse("2026-11-11T12:00:00Z"),
  });

  expect(counts.finished).toBe(1);
});
