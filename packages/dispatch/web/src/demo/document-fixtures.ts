// The ask card demo's documents (packages/dispatch/e2e/ask-card-demo/README.md): each issue's
// spec, as its live text and its versions read. Every word here is invented, as in fixtures.ts.
import type { ArtifactApprovalWithTrees } from "../api/types";
import {
  APPROVAL_ASK_ID,
  COLD_START,
  COLD_START_BLOCK_ID,
  DEMO_CLOCK,
  SPEC_MARKDOWN,
} from "./fixtures";

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

/** The spec of PANE-6, with its open decision as the check's rewrite left it: the document holds
 *  the rewritten question and options, and the ask keeps the wording the agent wrote. */
const COLD_START_MARKDOWN = `# Cold start

## Summary

When the mirror starts with no leases of its own, it rebuilds them from the ledger before it takes any new ones.

:::ask{#${COLD_START_BLOCK_ID} urgency="med"}
${COLD_START.question}

${COLD_START.options.map((option) => `- ${option.label}: ${option.description}`).join("\n")}
:::
`;

/** The spec of PANE-7 as the check rewrote it; the issue's `rewrite` keeps the agent's own
 *  (`AUDIT_ORIGINAL` in fixtures.ts). */
const AUDIT_MARKDOWN = `# Export the hand-back log as a CSV file for the weekly audit

## Summary

The weekly audit reads every lease the mirror handed back, but today the hand-back log that the lease mirror's spec asks for (its R4) is only in the service's logs. This change writes it out as a CSV file.

## What it does

- One file per ledger shard, with the columns of the hand-back log: the shard, the lease and the reason the ledger gave.
- It runs every Monday at 06:00, so the file is there before the audit review that morning.
- A lease the ledger refused and that was never handed back is left out, unless the export runs with \`--all\`.
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
  spec("PANE-6", COLD_START_MARKDOWN, ["Cold start", "Rewritten by the check"]),
  spec("PANE-7", AUDIT_MARKDOWN, ["Opened"]),
];

/** The spec's artifact id, as `issueSummary` and the approval requests name it. */
export function specId(issue: string): string {
  return `${issue}-spec`;
}

export function documentById(id: string): DemoDocument | undefined {
  return DOCUMENTS.find((document) => specId(document.issue) === id);
}
