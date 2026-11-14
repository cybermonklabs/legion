// The ask card demo's documents (packages/dispatch/e2e/ask-card-demo/README.md): each issue's
// spec, as its live text and its versions read. Every word here is invented, as in fixtures.ts.
import type { ArtifactApprovalWithTrees } from "../api/types";
import { APPROVAL_ASK_ID, DEMO_CLOCK, SPEC_MARKDOWN } from "./fixtures";

const hoursAgo = (hours: number) =>
  new Date(Date.parse(DEMO_CLOCK) - hours * 3_600_000).toISOString().replace(/\.000Z$/, "Z");

/** The spec of PANE-1, the version its approval request is for. */
const LEDGER_ROLLOUT_MARKDOWN = `# Ledger rollout: mirror first

## Summary

The ledger and the lease mirror move to the new cluster one day apart. The mirror moves first and replays against the old ledger until the ledger follows. While the ledger moves, the mirror hands a refused lease back at once instead of retrying it.

## Requirements

:::requirement{#r1}
**R1. Mirror first.** The mirror moves on Monday and replays from the old ledger until the ledger has moved.
:::

:::requirement{#r2}
**R2. One day apart.** The ledger moves on Tuesday, at least 24 hours after the mirror, so a replay that falls behind has a day to catch up.
:::

:::requirement{#r3}
**R3. Hand-back.** While the ledger moves, the mirror hands a refused lease back at once and asks for it again on its next cycle, 20 seconds later. The flip audit decides whether the hand-back stays once the move is over.
:::

:::requirement{#r4}
**R4. Way back.** If the ledger's move fails, the mirror moves back to the old cluster before the ledger's move is tried again.
:::
`;

const HANDOVER_MARKDOWN = `# Lease handover

## Summary

When a mirror stops, the leases it holds go back to the ledger in one batch, so the next holder does not wait for each lease to time out.
`;

/** One issue's spec: its text now, how many versions it has, and its approval. */
export interface DemoDocument {
  issue: string;
  markdown: string;
  /** The versions it has, oldest first: its live text is the last one's. */
  versions: { created_at: string; summary: string | null }[];
  approval?: ArtifactApprovalWithTrees;
}

function spec(
  issue: string,
  markdown: string,
  summaries: (string | null)[],
  approval?: ArtifactApprovalWithTrees
): DemoDocument {
  const count = summaries.length;
  return {
    issue,
    markdown,
    versions: summaries.map((summary, index) => ({
      created_at: hoursAgo(24 * (count - index)),
      summary,
    })),
    ...(approval === undefined ? {} : { approval }),
  };
}

function plain(issue: string, title: string): DemoDocument {
  return spec(
    issue,
    `# ${title}\n\n## Summary\n\nThe plan for this work is still being written.\n`,
    [null]
  );
}

export const DOCUMENTS: DemoDocument[] = [
  spec(
    "PANE-1",
    LEDGER_ROLLOUT_MARKDOWN,
    ["Ledger first", "Mirror first", "Way back", "Hand-back during the move"],
    { ask_id: APPROVAL_ASK_ID, latest_version: 4, state: "awaiting", version: 4 }
  ),
  plain("PANE-2", "Lease tool: dry-run flag"),
  plain("PANE-3", "Rollout order"),
  spec("PANE-4", HANDOVER_MARKDOWN, ["First hand-back plan"], {
    ask_id: "a4000000-0000-4000-8000-000000000004",
    latest_version: 1,
    state: "awaiting",
    version: 1,
  }),
  spec("PANE-5", SPEC_MARKDOWN, ["Retry for 60 seconds", "Renewal", "Flip audit"]),
  plain("PANE-6", "Cold start"),
];

/** The spec's artifact id, as `issueSummary` and the approval requests name it. */
export function specId(issue: string): string {
  return `${issue}-spec`;
}

export function documentById(id: string): DemoDocument | undefined {
  return DOCUMENTS.find((document) => specId(document.issue) === id);
}
