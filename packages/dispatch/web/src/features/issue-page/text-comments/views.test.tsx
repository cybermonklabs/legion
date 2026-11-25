import { afterEach, expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { waitFor } from "@testing-library/react";
import { SPEC_DOCUMENT } from "../decisions-filter/decisions-fixtures";
import { PRESET_NAMES, PRESET_ORDER, type Preset } from "../page-words";
import { blockNodeId } from "../tree-drafts";
import {
  answerSeededReads,
  columnOf,
  DECISION,
  drawPage,
  PLAN_DOCUMENT,
  PLAN_TASK,
  rowOf,
  STATEMENT,
  TEXT_THREADS,
} from "./margin-fixtures";

/**
 * R2: the column of comments on text stands in each of the six views of the page, beside the
 * words of the node a thread is on, and in no other view. A view the page adds later (Dashboard,
 * Threads and Documents, as their parts add them) is in neither list below until its part names
 * it, and the test fails until then.
 */

/** The margin's name for its column (`page-words-margin.ts`, R11). */
const COLUMN = "Comments on text";

/** The views whose stage mounts the column, and the views that mount none. */
const WITH_COLUMN: readonly Preset[] = [
  "conversation",
  "spec",
  "verification",
  "plan",
  "decisions",
  "graph",
];
const WITHOUT_COLUMN: readonly Preset[] = [];

/** The node each view draws that holds a seeded thread, by its tree id. */
const NODE_WITH_THREAD: Readonly<Record<Preset, string>> = {
  conversation: blockNodeId(SPEC_DOCUMENT.slug, STATEMENT),
  decisions: blockNodeId(SPEC_DOCUMENT.slug, DECISION),
  graph: blockNodeId(SPEC_DOCUMENT.slug, STATEMENT),
  plan: blockNodeId(PLAN_DOCUMENT.slug, PLAN_TASK),
  spec: blockNodeId(SPEC_DOCUMENT.slug, STATEMENT),
  verification: blockNodeId(SPEC_DOCUMENT.slug, STATEMENT),
};

let stubs: { mockRestore(): void }[] = [];
afterEach(() => {
  for (const stub of stubs) stub.mockRestore();
  stubs = [];
});

test("every view of R2 mounts the column and no other view does", async () => {
  stubs = answerSeededReads({ threads: TEXT_THREADS });
  const unnamed = PRESET_ORDER.filter(
    (preset) => !WITH_COLUMN.includes(preset) && !WITHOUT_COLUMN.includes(preset)
  );
  expect(
    unnamed.map((preset) => PRESET_NAMES[preset]),
    "a view of the page is in neither list of this test: name it"
  ).toEqual([]);
  const lacking: string[] = [];
  const unexpected: string[] = [];
  for (const preset of PRESET_ORDER) {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = drawPage(client, preset);
    try {
      await waitFor(() =>
        expect(
          rowOf(view, NODE_WITH_THREAD[preset]),
          `${PRESET_NAMES[preset]} draws no row for the node that holds the seeded thread`
        ).not.toBeNull()
      );
      const column = await columnOf(view, COLUMN);
      if (WITH_COLUMN.includes(preset) && column === null) lacking.push(PRESET_NAMES[preset]);
      if (WITHOUT_COLUMN.includes(preset) && column !== null) {
        unexpected.push(PRESET_NAMES[preset]);
      }
    } finally {
      view.finish();
      client.clear();
    }
  }
  expect(lacking, "the views whose stage mounts no column with a seeded thread").toEqual([]);
  expect(unexpected, "the views that mount a column and are no view of R2").toEqual([]);
});
