/** The ask card demo's fixture text (INFRA-3231's Decision 22), ported from the prototype's
 *  `web/src/demo/fixtures.ts` and `document-fixtures.ts` (branch `prototype/infra-3231-ask-card`
 *  at 50ba690369, and at b11c671eba for Quote reply's message and answered question): one
 *  invented project, PANE, with the questions, documents, rewrites and definitions the design's
 *  29 pictures show. `ask-card-pictures.e2e.ts` seeds it into the real stack and takes those
 *  pictures again from the build.
 *
 *  Every word here is invented; no production read went into it, as in the prototype. What the
 *  prototype's stub answered by itself - a rewrite, a definition, the asking agent's answer, the
 *  check's rewrite of a comment, an issue and an ask - is here as the text the stand-in model
 *  answers or the agent sends, so the server does the work the stub faked. */
import type { FakeSession } from "./agents";

/** The design's 29 pictures (INFRA-3231's spec, "The design as it runs"), each named as the
 *  demo's picture of the same screen is, at 1440x900 and, for the two phone ones, 390x844. */
export const PICTURES = [
  "ask-card-01-closed-inbox",
  "ask-card-02-menu-open",
  "ask-card-03-rewriting",
  "ask-card-04-rewritten",
  "ask-card-05-original-shown",
  "ask-card-06-rewrite-failed",
  "ask-card-07-reviewer-rewritten-inbox",
  "ask-card-08-define-ask-from-record",
  "ask-card-09-define-ask-with-agent",
  "ask-card-10-define-document-read-view",
  "ask-card-11-define-document-edit-bar",
  "ask-card-12-define-document-edit-view",
  "ask-card-13-shortcut-help",
  "ask-card-14-phone-card",
  "ask-card-15-phone-menu",
  "ask-card-16-approval-menu",
  "ask-card-17-reaches",
  "ask-card-18-block-rewritten",
  "ask-card-19-block-original",
  "ask-card-20-handed-on",
  "ask-card-21-comment-rewritten",
  "ask-card-22-comment-original",
  "ask-card-23-issue-rewritten",
  "ask-card-24-quote-select",
  "ask-card-25-quote-ask",
  "ask-card-26-quote-comment",
  "ask-card-27-quote-append",
  "ask-card-28-quote-message",
  "ask-card-29-quote-answered-thread",
] as const;
export type Picture = (typeof PICTURES)[number];

export const PROJECT = "PANE";
export const PROJECT_NAME = "Pane rollout";
export const READER = "alice";
/** The people of the demo besides the reader: a question is handed on by one to another. */
export const PEOPLE = ["bruno", "chen", "dana"] as const;

/** One session of the demo: its id, and the title the card names it by. */
export interface DemoSession {
  readonly id: string;
  readonly title: string;
}

/** The sessions that asked, and one that asked nothing and hears PANE-5 through a subscription
 *  to the issue. */
export const MIRROR: DemoSession = {
  id: "8b3f2c1e-4d5a-4f6b-9c7d-1e2f3a4b5c6d",
  title: "lease-mirror implementer",
};
export const TOOL: DemoSession = {
  id: "2a9e7d6c-5b4a-4c3d-8e2f-1a0b9c8d7e6f",
  title: "lease-tool implementer",
};
export const ORDER: DemoSession = {
  id: "c4d5e6f7-a8b9-4c0d-9e1f-2a3b4c5d6e7f",
  title: "rollout architect",
};
export const REVIEWER: DemoSession = {
  id: "5e6f7a8b-9c0d-4e1f-8a2b-3c4d5e6f7a8b",
  title: "lease-mirror reviewer",
};

/** Each session as the fake Envoy lists it: running on one machine, in the demo's directory,
 *  with `btw` where it takes a side question. */
export function liveSession(session: DemoSession, sideQuestions: boolean): FakeSession {
  return {
    capabilities: sideQuestions ? ["btw"] : [],
    dir: "/work/pane",
    host: "devbox",
    machine_id: "machine-1",
    session_id: session.id,
    title: session.title,
  };
}

/** The writes of a session, as the API takes its actor. */
export function asSession(session: DemoSession) {
  return {
    actor: { id: session.id, kind: "session" as const, origin: { session_title: session.title } },
    as: "agent" as const,
  };
}

/** One issue of the demo: its key in creation order, its title, priority and status, and how
 *  many minutes ago it was opened. */
export interface DemoIssue {
  readonly key: string;
  readonly title: string;
  readonly priority: number | null;
  readonly status: string;
  readonly openedMinutesAgo: number;
}

const THREE_DAYS = 60 * 24 * 3;

export const ISSUES: readonly DemoIssue[] = [
  {
    key: "PANE-1",
    openedMinutesAgo: THREE_DAYS,
    priority: 1,
    status: "needs_review",
    title: "Ledger rollout",
  },
  {
    key: "PANE-2",
    openedMinutesAgo: THREE_DAYS,
    priority: 3,
    status: "in_progress",
    title: "Lease tool: dry-run flag",
  },
  {
    key: "PANE-3",
    openedMinutesAgo: THREE_DAYS,
    priority: 0,
    status: "todo",
    title: "Rollout order",
  },
  {
    key: "PANE-4",
    openedMinutesAgo: THREE_DAYS,
    priority: 2,
    status: "todo",
    title: "Lease handover",
  },
  {
    key: "PANE-5",
    openedMinutesAgo: THREE_DAYS,
    priority: 3,
    status: "in_progress",
    title: "Lease mirror: refused-lease retries",
  },
  {
    key: "PANE-6",
    openedMinutesAgo: THREE_DAYS,
    priority: 3,
    status: "backlog",
    title: "Cold start",
  },
];

/** The spec of an issue whose plan is still being written. */
export function plainSpec(title: string): string {
  return `# ${title}\n\n## Summary\n\nThe plan for this work is still being written.\n`;
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

/** The spec of PANE-1, the version its approval request is for. */
export const LEDGER_ROLLOUT_MARKDOWN = `# Ledger rollout: mirror first

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

/** PANE-1's spec has four versions, the fourth of which is LEDGER_ROLLOUT_MARKDOWN and the one
 *  its approval request is for: each version's summary, oldest first. The prototype read every
 *  past version as the current text; the build keeps each version's own, so the three before the
 *  last are the plan as it grew, one requirement at a time. */
export const LEDGER_ROLLOUT_VERSIONS: readonly { summary: string; markdown: string }[] = [
  {
    markdown: LEDGER_ROLLOUT_MARKDOWN.replace("mirror first", "ledger first").split(
      "\n## Requirements"
    )[0],
    summary: "Ledger first",
  },
  {
    markdown: LEDGER_ROLLOUT_MARKDOWN.split(":::requirement{#r3}")[0],
    summary: "Mirror first",
  },
  {
    markdown: LEDGER_ROLLOUT_MARKDOWN.replace(/:::requirement\{#r3\}\n[^\n]*\n:::\n\n/, ""),
    summary: "Way back",
  },
  { markdown: LEDGER_ROLLOUT_MARKDOWN, summary: "Hand-back during the move" },
];

export const HANDOVER_MARKDOWN = `# Lease handover

## Summary

When a mirror stops, the leases it holds go back to the ledger in one batch, so the next holder does not wait for each lease to time out.
`;

/** One question's wording: its question and options. */
export interface Wording {
  readonly question: string;
  readonly options: readonly { label: string; description?: string }[];
}

/** The decision block in PANE-6's spec: the check rewrote it, so the document holds the rewritten
 *  question and options and the agent's own wording is a key away. */
export const COLD_START_BLOCK = "cold-start";
export const COLD_START_ORIGINAL: Wording = {
  options: [{ label: "Full replay" }, { label: "From the snapshot" }],
  question: "Cold start: full replay or snap + tail?",
};
export const COLD_START_REWRITTEN: Wording = {
  options: [
    {
      description: "The mirror replays every lease from the ledger's start, about 40 minutes.",
      label: "Full replay",
    },
    {
      description:
        "The mirror loads the last snapshot and replays what came after it, about 4 minutes.",
      label: "From the snapshot",
    },
  ],
  question:
    "Should a cold start of the mirror replay the whole ledger, or load the last snapshot and replay only what came after it?\n\nA full replay takes about 40 minutes on today's ledger. The mirror writes a snapshot every 6 hours, so loading one leaves at most 6 hours of leases to replay, about 4 minutes.\n\n**Recommended: from the snapshot.** A snapshot holds every lease a full replay would rebuild, and the mirror is back 36 minutes sooner.",
};

/** The spec of PANE-6 as the lease-mirror implementer wrote it, before the check's rewrite of its
 *  decision block, and the block as it wrote it. */
export const COLD_START_MARKDOWN = `# Cold start

## Summary

When the mirror starts with no leases of its own, it rebuilds them from the ledger before it takes any new ones.
`;
export const COLD_START_BLOCK_MARKDOWN = `:::ask{#${COLD_START_BLOCK} urgency="med" multiple="false" state="open"}
${COLD_START_ORIGINAL.question}

${COLD_START_ORIGINAL.options.map((option) => `- ${option.label}`).join("\n")}
:::
`;

/** The ask the pictures turn on: short, written mid-work, with words only its agent knows, and
 *  the rewrite a person's Rewrite gives it. */
export const FLIP: Wording & { readonly minutesAgo: number } = {
  minutesAgo: 42,
  options: [{ label: "Keep" }, { label: "Flip" }],
  question: "R3 flip audit: go with B per the train?",
};
export const FLIP_REWRITTEN: Wording = {
  options: [
    {
      description: "The mirror keeps retrying a refused lease for up to 60 seconds.",
      label: "Keep",
    },
    {
      description: "The mirror hands a refused lease back and asks again 20 seconds later.",
      label: "Flip",
    },
  ],
  question:
    "When the ledger refuses a lease, should the mirror keep retrying it, or hand it back at once and ask again on its next cycle?\n\nThe lease handler ships once this is decided. Last week the mirror retried 412 refused leases; 9 later went through, all within 30 seconds, and each retry held a ledger connection for up to 5 seconds.\n\n**Recommended: Flip.** Those 9 would also go through on the next cycle, and handing back frees about 30 minutes of ledger connection time a day.",
};

/** The ask whose rewrite fails, as one that changed an option's label would. */
export const ORDER_ASK: Wording = {
  options: [{ label: "Ledger first" }, { label: "Mirror first" }],
  question: "Which service moves first?",
};
/** The failed rewrite: it renames the first option, which a rewrite keeps byte for byte. */
export const ORDER_BROKEN_REWRITE: Wording = {
  options: [
    {
      description: "The ledger moves on Monday and the mirror on Tuesday.",
      label: "The ledger first",
    },
    {
      description: "The mirror moves on Monday and replays against the old ledger until Tuesday.",
      label: "Mirror first",
    },
  ],
  question:
    "Which service moves to the new cluster first: the ledger or the mirror?\n\nThe move starts on Monday, and the two cannot move on the same day, because the mirror reads the ledger's leases while it replays. Whichever moves second is down for about 20 minutes while it reconnects.\n\n**Recommended: ledger first.** The mirror can replay from a moved ledger; the ledger cannot wait on a moved mirror.",
};

/** The approval requests: the summary each was asked with, which the server words as "Approve
 *  spec.md (version N)? <summary>". */
export const LEDGER_APPROVAL_SUMMARY =
  "The mirror-first plan, with the hand-back of R3 and its audit.";
export const HANDOVER_APPROVAL_SUMMARY = "The first hand-back plan.";

/** The ask Dispatch's check rewrote before it reached the Inbox: the lease-tool implementer's
 *  wording, and the check's. */
export const DRY_RUN_ORIGINAL: Wording = {
  options: [{ label: "Ship as is" }, { label: "Hold" }],
  question: "Ship `--dry-run` as is, or hold it for the R3 flip audit?",
};
export const DRY_RUN_REWRITTEN: Wording = {
  options: [
    {
      description: "The flag ships on Monday and prints today's retries until the audit closes.",
      label: "Ship as is",
    },
    {
      description: "The flag waits for the release after the audit, about two weeks.",
      label: "Hold",
    },
  ],
  question:
    "Should the lease tool's new `--dry-run` flag ship in Monday's release, or wait until the R3 audit has closed?\n\n`--dry-run` prints what the tool would hand back and writes nothing. The R3 audit decides whether the mirror retries a refused lease or hands it back at once, so until it closes the flag prints today's retries.\n\n**Recommended: ship it as is.** It writes nothing, so the audit cannot make it wrong, only out of date for a week.",
};

/** The question one person handed to another: it waits on bruno, and dana handed it on. */
export const HANDED_ON: Wording = {
  options: [{ label: "One batch" }, { label: "Split by shard" }],
  question: "Can the hand-back batch hold 500 leases, or should it split them by shard?",
};

/** The comment on PANE-5 the check rewrote before anyone read it: the agent's and the check's. */
export const COMMENT_ORIGINAL = "R3 audit done: 9/412 came good <30s, so B per the train. see ask.";
export const COMMENT_REWRITTEN =
  "The flip audit is done. Of last week's 412 refused leases, 9 went through on a retry, all within 30 seconds, so a retry wins back very little. I recommend Flip in the open question: hand a refused lease back at once and ask again on the next cycle.";

/** Quote reply's pictures (24 to 29): what each quote puts in its box, as the demo's run checked
 *  it. Two paragraphs of the rewritten dry-run question, the recommendation's bold kept. */
export const DRY_RUN_QUOTE =
  "> The R3 audit decides whether the mirror retries a refused lease or hands it back at once, so until it closes the flag prints today's retries.\n>\n> **Recommended: ship it as is.** It writes nothing, so the audit cannot make it wrong, only out of date for a week.\n\n";
/** A sentence of the rewritten comment, in its thread's reply box. */
export const COMMENT_QUOTE =
  "> Of last week's 412 refused leases, 9 went through on a retry, all within 30 seconds, so a retry wins back very little.\n\n";
/** The line the reader typed in the flip audit's answer box before quoting its question. */
export const APPEND_TYPED = "Flip. One thing I can't read here:";
export const APPEND_QUOTE = `${APPEND_TYPED}\n\n> go with B per the train?\n\n`;

/** The rollout architect's note on PANE-3's Conversation tab, written 95 minutes before the
 *  pictures' moment, and its middle paragraph quoted in the tab's composer. */
export const ORDER_MESSAGE = {
  body: "The cluster team moved the window. The ledger can move on Monday only before 10:00; after that, its next slot is Thursday.\n\nThe mirror can move on any day, so if the ledger misses Monday morning, the mirror goes first and the ledger follows on Thursday.\n\nI will keep the order the open question settles on unless the ledger misses its slot.",
  minutesAgo: 95,
};
export const MESSAGE_QUOTE =
  "> The mirror can move on any day, so if the ledger misses Monday morning, the mirror goes first and the ledger follows on Thursday.\n\n";

/** The question on PANE-2 the reader answered in her own words three hours ago, asked an hour
 *  before that, and the lease-tool implementer's reply under the answer an hour after it, whose
 *  list, code and bold a Quote reply keeps. */
export const ANSWERED = {
  answer: "One line for each: why the tool leaves it alone, and the lease's id.",
  answeredMinutesAgo: 60 * 3,
  askedMinutesAgo: 60 * 4,
  question: "What should `--dry-run` print for a lease the tool leaves alone?",
  reply:
    "Done in the next build. After the leases it would hand back, `--dry-run` now prints:\n\n- `skip` and the lease's id, for a lease another holder has\n- `stale` and the id, for a lease past its renewal\n\n**One thing changed:** the summary line counts the skipped leases too.",
  repliedMinutesAgo: 60 * 2,
};
export const ANSWERED_QUOTE =
  "> - `skip` and the lease's id, for a lease another holder has\n>\n> - `stale` and the id, for a lease past its renewal\n>\n> **One thing changed:** the summary line counts the skipped leases too.\n\n";

/** The issue the lease-tool implementer opened, whose title and spec the check rewrote: its own
 *  and the check's. */
export const AUDIT_ORIGINAL = {
  spec: `# R4 log → csv for wkly audit (hb rows)

Need hb rows out as csv for the wkly audit, same cols as R4, per shard. Cron it Mon 06:00 so it's there before the review. Skip refused-only rows unless --all.
`,
  title: "R4 log → csv for wkly audit (hb rows)",
};
export const AUDIT_REWRITTEN = {
  spec: `# Export the hand-back log as a CSV file for the weekly audit

## Summary

The weekly audit reads every lease the mirror handed back, but today the hand-back log that the lease mirror's spec asks for (its R4) is only in the service's logs. This change writes it out as a CSV file.

## What it does

- One file per ledger shard, with the columns of the hand-back log: the shard, the lease and the reason the ledger gave.
- It runs every Monday at 06:00, so the file is there before the audit review that morning.
- A lease the ledger refused and that was never handed back is left out, unless the export runs with \`--all\`.
`,
  title: "Export the hand-back log as a CSV file for the weekly audit",
};

/** What Define answers, by the words the reader selected: the meaning from the record, the
 *  requirement of PANE-5's spec that defines it, and the asking agent's own words where it gives
 *  them. */
export interface DemoDefinition {
  readonly words: string;
  readonly meaning: string;
  /** The requirement of PANE-5's spec the definition cites, by its block id. */
  readonly requirement: string;
  readonly agent?: string;
}

export const FLIP_AUDIT: DemoDefinition = {
  agent:
    "The week of refused leases I counted before choosing between retrying and handing back. 412 were refused and only 9 came good on a retry, which is why I lean to Flip.",
  meaning:
    "The check R3 of the spec asks for before the lease handler ships: whether the mirror should stop retrying a refused lease and hand it back at once. “Flip” is that change.",
  requirement: "r3",
  words: "flip audit",
};
export const REFUSED_LEASE: DemoDefinition = {
  meaning:
    "A lease the ledger would not grant, because another holder has it or the shard is moving. The mirror asks for one every 20 seconds while it replays.",
  requirement: "r2",
  words: "refused lease",
};
