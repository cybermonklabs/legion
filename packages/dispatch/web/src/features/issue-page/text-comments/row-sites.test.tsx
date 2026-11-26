import { afterEach, describe, expect, test } from "bun:test";
import { type RenderResult, waitFor } from "@testing-library/react";

import type { Anchor } from "../../../api/types";
import type { Preset } from "../page-words";
import { GATES, type GateName } from "./gates";
import {
  answerSeededReads,
  drawPage,
  newClient,
  ON_CHECK,
  ON_DECISION,
  ON_PLAN_TASK,
  ON_STATEMENT,
  readAgain,
  type TextThreadSeed,
} from "./margin-fixtures";
import { rowTextOf } from "./row-text-runs";

/**
 * R3 and R5: each component that draws one node's own words in a view of R2 (a row site) names
 * one words element, which the quote's reading starts from (`row-text-runs.ts`), and draws the
 * node's threads in the sheet below 1024 px.
 *
 * The sites are those of the plan's list that draw one node's words, found in the code at this
 * head: the lines the plan names moved or named a row that holds none. `PlanFilterBody.tsx`'s
 * `CycleRow` (a cycle's one line, a link to each of its tasks) and `VerificationFilterBody.tsx`'s
 * issue row (the counts of the issue's statements) draw no node's words, so they are no site and
 * take no control or sheet; the Plan view's list of steps draws `NodeRow`.
 */

/** A row site: the view that draws it, the selector that finds one of its rows, the seeded thread
 *  on the node it draws, the facts element of the row that the sheet stands after, and the part
 *  whose row it is (the sheet is mounted after that part, G6). */
interface Site {
  /** The element of the row that holds its facts, where it draws any. */
  readonly facts?: string;
  readonly gate: GateName;
  readonly name: string;
  readonly preset: Preset;
  readonly row: string;
  readonly thread: TextThreadSeed;
}

const SITES: readonly Site[] = [
  {
    gate: "PART_X",
    name: "NodeRow.tsx in Conversation",
    preset: "conversation",
    row: '[data-issue-body] [data-node-row][data-node-type="requirement"]',
    thread: ON_STATEMENT,
  },
  {
    gate: "PART_X",
    name: "NodeRow.tsx in Graph",
    preset: "graph",
    row: '[data-issue-body] [data-node-row][data-node-type="requirement"]',
    thread: ON_STATEMENT,
  },
  {
    gate: "PART_U",
    name: "DecisionsRow.tsx",
    preset: "decisions",
    row: "[data-decisions-body] [data-node-row]",
    thread: ON_DECISION,
  },
  {
    facts: "[data-plan-facts]",
    gate: "PART_X",
    name: "PlanFilterBody.tsx, a step of the chart",
    preset: "plan",
    row: "[data-plan-bar-row]",
    thread: ON_PLAN_TASK,
  },
  {
    gate: "PART_J",
    name: "SpecFilterBody.tsx, the decision cell",
    preset: "spec",
    row: "[data-spec-cell]",
    thread: ON_DECISION,
  },
  {
    gate: "PART_J",
    name: "SpecFilterBody.tsx, a statement",
    preset: "spec",
    row: "[data-spec-statement]",
    thread: ON_STATEMENT,
  },
  {
    gate: "PART_J",
    name: "SpecFilterBody.tsx, a row that is no statement",
    preset: "spec",
    row: "[data-spec-other]",
    thread: ON_STATEMENT,
  },
  {
    gate: "PART_Y",
    name: "VerificationFilterBody.tsx, a statement",
    preset: "verification",
    row: "[data-verification-card]",
    thread: ON_STATEMENT,
  },
  {
    gate: "PART_Y",
    name: "VerificationFilterBody.tsx, a check",
    preset: "verification",
    row: "[data-verification-check]",
    thread: ON_CHECK,
  },
  {
    gate: "PART_Y",
    name: "VerificationFilterBody.tsx, a result",
    preset: "verification",
    row: "[data-verification-result]",
    thread: ON_STATEMENT,
  },
  {
    gate: "PART_Y",
    name: "VerificationFilterBody.tsx, a row that is no statement or check",
    preset: "verification",
    row: "[data-verification-other]",
    thread: ON_STATEMENT,
  },
];

/** Where the sheet stands, below 1024 px. */
const BELOW_PANE = 1000;

let stubs: { mockRestore(): void }[] = [];
afterEach(() => {
  for (const stub of stubs) stub.mockRestore();
  stubs = [];
});

/** The first row `site` draws in `view`, once it is drawn. */
async function rowOfSite(view: RenderResult, site: Site): Promise<HTMLElement> {
  return waitFor(
    () => {
      const row = view.container.querySelector<HTMLElement>(site.row);
      expect(row, `${site.name} draws no row, by ${site.row}`).not.toBeNull();
      return row as HTMLElement;
    },
    { timeout: 4000 }
  );
}

/** The words elements that are the row's own: those no row nested in it holds (a statement of the
 *  Verification view draws its checks, and a check its latest result, inside its row). */
function ownWordsOf(row: HTMLElement): HTMLElement[] {
  return [...row.querySelectorAll<HTMLElement>("[data-node-words]")].filter(
    (words) => words.closest("[data-node-id]") === row
  );
}

describe("each row site names one words element", () => {
  for (const site of SITES) {
    test(site.name, async () => {
      stubs = answerSeededReads({ threads: [site.thread] });
      const client = newClient();
      const view = drawPage(client, site.preset);
      try {
        const row = await rowOfSite(view, site);
        const own = ownWordsOf(row);
        expect(own.length, `${site.name} draws not one [data-node-words] of its own`).toBe(1);
        expect(rowTextOf(row), `${site.name}'s words element is not the row's own`).toBe(own[0]);
      } finally {
        view.finish();
        client.clear();
      }
    });
  }
});

describe("every row site draws its node's threads in the sheet below 1024 px", () => {
  for (const site of SITES) {
    test.skipIf(!GATES[site.gate])(site.name, async () => {
      stubs = answerSeededReads({ threads: [site.thread] });
      const client = newClient();
      const view = drawPage(client, site.preset, { width: BELOW_PANE });
      try {
        const row = await rowOfSite(view, site);
        await readAgain(client);
        const sheet = await waitFor(() => {
          const drawn = row.querySelector<HTMLElement>("[data-text-threads]");
          expect(drawn, `${site.name} draws no [data-text-threads] in its row`).not.toBeNull();
          return drawn as HTMLElement;
        });
        const after = [ownWordsOf(row)[0], site.facts ? row.querySelector(site.facts) : null];
        for (const before of after) {
          if (before === null || before === undefined) continue;
          expect(
            before.compareDocumentPosition(sheet) & Node.DOCUMENT_POSITION_FOLLOWING,
            `the sheet stands before ${site.name}'s words or facts`
          ).not.toBe(0);
        }
        expect(sheet.textContent, "the sheet holds not the seeded thread").toContain(
          site.thread.comment.body
        );
      } finally {
        view.finish();
        client.clear();
      }
    });
  }
});

/**
 * A result takes a thread through a node anchor (G2, `anchor.node`), which no read at B0 answers,
 * so its sheet case stands apart: owed on part Y for the row and on the node anchor for the
 * thread. The tree read hangs the comment under the result's node with the anchor naming it.
 */
test.skipIf(!GATES.PART_Y || !GATES.NODE_ANCHOR)(
  "a result's thread stands in the sheet of the result's row below 1024 px",
  async () => {
    const onResult = (thread: TextThreadSeed): TextThreadSeed => ({
      ...thread,
      comment: {
        ...thread.comment,
        // The node anchor of 1253 R31, which the generated `Anchor` type does not hold yet.
        anchor: {
          ...(thread.comment.anchor as Anchor),
          node: { id: "res1" },
        } as unknown as Anchor,
      },
      node: "res1",
    });
    stubs = answerSeededReads({ threads: [onResult(ON_CHECK)] });
    const client = newClient();
    const view = drawPage(client, "verification", { width: BELOW_PANE });
    try {
      const row = await waitFor(() => {
        const drawn = view.container.querySelector<HTMLElement>("[data-verification-result]");
        expect(drawn, "the Verification view draws no result row").not.toBeNull();
        return drawn as HTMLElement;
      });
      await waitFor(() =>
        expect(
          row.querySelector("[data-text-threads]"),
          "the result's row draws no sheet"
        ).not.toBeNull()
      );
    } finally {
      view.finish();
      client.clear();
    }
  }
);
