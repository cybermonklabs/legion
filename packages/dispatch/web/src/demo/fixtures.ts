// The ask card demo's fixtures (packages/dispatch/e2e/ask-card-demo/README.md): one invented
// project, PANE, with the open questions and documents the pictures show. Every word here is
// invented; no production read went into it, so the fixtures live in the repository and the
// single file inlines them with the app.
import type { Agent, InboxRow, IssueSummary, Project } from "../api/types";
import type { AskRewrite } from "../features/inbox/ask-rewrite";

/** The moment the page's clock starts from (`clock.ts`), so every age reads the same. */
export const DEMO_CLOCK = "2026-11-14T15:00:00Z";
export const READER = "alice";
export const PROJECT = "PANE";

const minutesAgo = (minutes: number) =>
  new Date(Date.parse(DEMO_CLOCK) - minutes * 60_000).toISOString().replace(/\.000Z$/, "Z");

/** The sessions that asked. */
const MIRROR = {
  id: "8b3f2c1e-4d5a-4f6b-9c7d-1e2f3a4b5c6d",
  kind: "session",
  origin: { session_title: "lease-mirror implementer" },
} as const;
const TOOL = {
  id: "2a9e7d6c-5b4a-4c3d-8e2f-1a0b9c8d7e6f",
  kind: "session",
  origin: { session_title: "lease-tool implementer" },
} as const;
const ORDER = {
  id: "c4d5e6f7-a8b9-4c0d-9e1f-2a3b4c5d6e7f",
  kind: "session",
  origin: { session_title: "rollout architect" },
} as const;

export const AGENTS: Agent[] = [MIRROR, TOOL, ORDER].map((actor, index) => ({
  capabilities: ["btw"],
  dir: "/work/pane",
  host: "devbox",
  last_activity: minutesAgo(2 + index),
  last_seen: Math.floor(Date.parse(DEMO_CLOCK) / 1000) - 30,
  machine_id: "machine-1",
  open_asks: 1,
  roles: [],
  session_id: actor.id,
  state: "running",
  title: actor.origin.session_title,
}));

export const USERS = ["alice", "bruno", "chen", "dana"];

export const PROJECTS: Project[] = [
  {
    created_at: minutesAgo(60 * 24 * 30),
    default_assignee: READER,
    description: "",
    key: PROJECT,
    name: "Pane rollout",
    next_issue_number: 7,
  } as unknown as Project,
];

interface DemoIssue {
  key: string;
  title: string;
  priority: number | null;
  status: string;
}

export const ISSUES: DemoIssue[] = [
  { key: "PANE-1", title: "Ledger rollout", priority: 1, status: "needs_review" },
  { key: "PANE-2", title: "Lease tool: dry-run flag", priority: 3, status: "in_progress" },
  { key: "PANE-3", title: "Rollout order", priority: 0, status: "todo" },
  { key: "PANE-4", title: "Lease handover", priority: 2, status: "todo" },
  {
    key: "PANE-5",
    title: "Lease mirror: refused-lease retries",
    priority: 3,
    status: "in_progress",
  },
  { key: "PANE-6", title: "Cold start", priority: 3, status: "backlog" },
];

export function issueSummary(issue: DemoIssue, openAsks: number): IssueSummary {
  return {
    assignee: READER,
    blocked_by: [],
    claim: null,
    closed_at: null,
    components: [],
    created_at: minutesAgo(60 * 24 * 3),
    created_by: { id: READER, kind: "user" },
    key: issue.key,
    labels: [],
    last_seq: 10,
    open_asks: openAsks,
    parent: null,
    primary_artifact_id: `${issue.key}-spec`,
    priority: issue.priority,
    progress: null,
    rank: issue.key.slice(-1),
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    status: issue.status,
    title: issue.title,
    updated_at: minutesAgo(5),
  } as unknown as IssueSummary;
}

const EMPTY_THREAD = {
  answers: [],
  claimant: null,
  edits: [],
  followers: [],
  replies: [],
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
};

/** An Inbox row as the stub answers it: the read the design adds a rewrite to. */
export type DemoRow = InboxRow & { rewrite?: AskRewrite };

/** One open question of the demo, as the Inbox row carries it, with the rewrite the stub gives
 *  it when the reader presses Rewrite (`rewriteTo`) and, for the ask the check rewrote, the
 *  wording the agent wrote (`rewrite`). */
export interface DemoAsk {
  row: DemoRow;
  rewriteTo?: { question: string; options: { label: string; description?: string }[] };
  /** The first rewrite fails, as one that changed an option's label would, and a second one
   *  goes through. */
  failFirstRewrite?: boolean;
}

function ask(
  id: string,
  issueKey: string,
  author: InboxRow["author"],
  minutes: number,
  fields: Partial<DemoRow>
): DemoRow {
  const issue = ISSUES.find((one) => one.key === issueKey);
  return {
    anchor: null,
    answer: null,
    artifact_id: null,
    asked_of: null,
    asked_of_at: null,
    asked_of_by: null,
    author,
    block_id: null,
    created_at: minutesAgo(minutes),
    edited_at: null,
    id,
    issue: { assignee: READER, key: issueKey, title: issue?.title ?? "" },
    issue_key: issueKey,
    kind: "question",
    last_reply: null,
    multiple: false,
    opened_event_id: 1,
    options: [],
    priority: issue?.priority ?? null,
    question: "",
    snoozed_until: null,
    state: "open",
    thread: EMPTY_THREAD,
    urgency: "med",
    waiting_on: "human",
    waiting_on_person: READER,
    ...fields,
  } as DemoRow;
}

/** The ask the pictures turn on: short, written mid-work, with words only its agent knows. */
export const FLIP_ASK_ID = "a5000000-0000-4000-8000-000000000005";
/** The ask the check rewrote before it reached the Inbox. */
export const CHECKED_ASK_ID = "a2000000-0000-4000-8000-000000000002";
/** The ask whose first rewrite fails. */
export const ORDER_ASK_ID = "a3000000-0000-4000-8000-000000000003";
/** The approval request. */
export const APPROVAL_ASK_ID = "a1000000-0000-4000-8000-000000000001";

const DRY_RUN_ORIGINAL = {
  question: "Ship `--dry-run` as is, or hold it for the R3 flip audit?",
  options: [{ label: "Ship as is" }, { label: "Hold" }],
};

export const ASKS: DemoAsk[] = [
  {
    row: ask(ORDER_ASK_ID, "PANE-3", ORDER, 60 * 23, {
      question: "Which service moves first?",
      options: [{ label: "Ledger first" }, { label: "Mirror first" }],
      urgency: "med",
    }),
    failFirstRewrite: true,
    rewriteTo: {
      question:
        "Which service moves to the new cluster first: the ledger or the mirror?\n\nThe move starts on Monday, and the two cannot move on the same day, because the mirror reads the ledger's leases while it replays. Whichever moves second is down for about 20 minutes while it reconnects.\n\n**Recommended: ledger first.** The mirror can replay from a moved ledger; the ledger cannot wait on a moved mirror.",
      options: [
        {
          label: "Ledger first",
          description: "The ledger moves on Monday and the mirror on Tuesday.",
        },
        {
          label: "Mirror first",
          description:
            "The mirror moves on Monday and replays against the old ledger until Tuesday.",
        },
      ],
    },
  },
  {
    row: ask(APPROVAL_ASK_ID, "PANE-1", ORDER, 60 * 23, {
      kind: "approval",
      question:
        "Approve spec.md (version 4)? The mirror-first plan, with the hand-back of R3 and its audit.",
      options: [{ label: "Approve" }, { label: "Request changes" }],
      urgency: "high",
      approval: {
        artifact_id: "PANE-1-spec",
        documents: [],
        name: "spec.md",
        requested_version: 4,
        version: 4,
      },
      approval_artifact: { name: "spec.md", primary: true, project: PROJECT, slug: "spec-md" },
    }),
  },
  {
    row: ask("a4000000-0000-4000-8000-000000000004", "PANE-4", ORDER, 60 * 23, {
      kind: "approval",
      question: "Approve spec.md (version 1)? The first hand-back plan.",
      options: [{ label: "Approve" }, { label: "Request changes" }],
      urgency: "high",
      approval: {
        artifact_id: "PANE-4-spec",
        documents: [],
        name: "spec.md",
        requested_version: 1,
        version: 1,
      },
      approval_artifact: { name: "spec.md", primary: true, project: PROJECT, slug: "spec-md" },
    }),
  },
  {
    row: ask(CHECKED_ASK_ID, "PANE-2", TOOL, 7, {
      question:
        "Should the lease tool's new `--dry-run` flag ship in Monday's release, or wait until the R3 audit has closed?\n\n`--dry-run` prints what the tool would hand back and writes nothing. The R3 audit decides whether the mirror retries a refused lease or hands it back at once, so until it closes the flag prints today's retries.\n\n**Recommended: ship it as is.** It writes nothing, so the audit cannot make it wrong, only out of date for a week.",
      options: [
        {
          label: "Ship as is",
          description:
            "The flag ships on Monday and prints today's retries until the audit closes.",
        },
        {
          label: "Hold",
          description: "The flag waits for the release after the audit, about two weeks.",
        },
      ],
      urgency: "med",
      edited_at: minutesAgo(6),
      rewrite: {
        by: "check",
        state: "done",
        at: minutesAgo(6),
        original: DRY_RUN_ORIGINAL,
      },
    }),
  },
  {
    row: ask(FLIP_ASK_ID, "PANE-5", MIRROR, 42, {
      question: "R3 flip audit: go with B per the train?",
      options: [{ label: "Keep" }, { label: "Flip" }],
      urgency: "high",
    }),
    rewriteTo: {
      question:
        "When the ledger refuses a lease, should the mirror keep retrying it, or hand it back at once and ask again on its next cycle?\n\nThe lease handler ships once this is decided. Last week the mirror retried 412 refused leases; 9 later went through, all within 30 seconds, and each retry held a ledger connection for up to 5 seconds.\n\n**Recommended: Flip.** Those 9 would also go through on the next cycle, and handing back frees about 30 minutes of ledger connection time a day.",
      options: [
        {
          label: "Keep",
          description: "The mirror keeps retrying a refused lease for up to 60 seconds.",
        },
        {
          label: "Flip",
          description: "The mirror hands a refused lease back and asks again 20 seconds later.",
        },
      ],
    },
  },
  {
    row: ask("a6000000-0000-4000-8000-000000000006", "PANE-6", MIRROR, 60 * 23, {
      question: "Does a cold start replay the whole ledger?",
      options: [{ label: "Yes" }, { label: "No, from the last snapshot" }],
      urgency: "med",
    }),
  },
];

/** What Define answers, by the words the reader selected: the meaning from the record, where
 *  the documents define it, and the asking agent's own words. */
export interface DemoDefinition {
  words: string;
  meaning: string;
  link?: { label: string; href: string };
  agent?: { actor: InboxRow["author"]; answer: string };
}

export const DEFINITIONS: DemoDefinition[] = [
  {
    words: "flip audit",
    meaning:
      "The check R3 of the spec asks for before the lease handler ships: whether the mirror should stop retrying a refused lease and hand it back at once. “Flip” is that change.",
    link: { label: "PANE-5 spec · R3", href: "/issues/PANE-5/spec#r3" },
    agent: {
      actor: MIRROR,
      answer:
        "The week of refused leases I counted before choosing between retrying and handing back. 412 were refused and only 9 came good on a retry, which is why I lean to Flip.",
    },
  },
  {
    words: "the train",
    meaning:
      "The comment thread on this issue’s spec where the two plans for a refused lease were discussed. B is its second plan, the hand-back.",
    link: { label: "PANE-5 spec · Comments", href: "/issues/PANE-5/spec" },
    agent: {
      actor: MIRROR,
      answer: "The thread under R3 in the spec. Plan B there is the hand-back.",
    },
  },
  {
    words: "refused lease",
    meaning:
      "A lease the ledger would not grant, because another holder has it or the shard is moving. The mirror asks for one every 20 seconds while it replays.",
    link: { label: "PANE-5 spec · R2", href: "/issues/PANE-5/spec#r2" },
  },
];

/** The fallback for words the fixtures do not define. */
export function definitionOf(words: string): DemoDefinition {
  const wanted = words.trim().toLowerCase();
  return (
    DEFINITIONS.find((one) => wanted.includes(one.words) || one.words.includes(wanted)) ?? {
      words,
      meaning: `No document of this issue defines “${words}”; read here, it is used in its everyday sense.`,
    }
  );
}

/** The spec of PANE-5, as its document reads: what Define reads in the read and the edit views. */
export const SPEC_MARKDOWN = `# Lease mirror: refused-lease retries

## Summary

The mirror replays the ledger's leases onto the standby cluster. When the ledger refuses a lease, the mirror retries it for up to 60 seconds. This change decides whether it keeps doing so.

## Requirements

:::requirement{#r1}
**R1. One lease per shard.** The mirror holds at most one lease per ledger shard at a time.
:::

:::requirement{#r2}
**R2. Renewal.** A lease the ledger grants is renewed every 20 seconds while the mirror replays. A refused lease is one the ledger would not grant, because another holder has it or the shard is moving.
:::

:::requirement{#r3}
**R3. Flip audit.** Before the lease handler ships, the team audits whether the mirror flips from retrying a refused lease to handing it back at once, over one week of refused leases, and records the outcome in this section.
:::

:::requirement{#r4}
**R4. Hand-back log.** Each hand-back is logged with the shard, the lease and the reason the ledger gave.
:::
`;
