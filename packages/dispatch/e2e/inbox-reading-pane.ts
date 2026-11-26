/** The Inbox's reading column as its rows read it: the questions the column is seeded with, the
 *  two columns it draws, and where a block stands in the scrolled one. Shared by
 *  `inbox-reading-pane.e2e.ts` and the rows of that family which run against a recording
 *  (`inbox-reading-pane.stub.e2e.ts`).
 *
 *  Every document here is made up: this repository is public, so nothing names a real person,
 *  host or project.
 */
import { expect, type Page } from "@playwright/test";
import type { InboxRow } from "../web/src/api/types";

import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  createProjectDocument,
  editArtifact,
  getArtifact,
  getArtifactVersion,
  getInbox,
  handAskOn,
  listMyReplies,
  patchIssue,
  requestApproval,
  withOpenedAsk,
} from "./api";
import { blockId } from "./editor";
import { inboxList, inboxPane } from "./inbox-pane";
import { replyRowId } from "./inbox-threads";

export const session = {
  actor: { id: "e2e-reading-pane", kind: "session" as const },
  as: "agent" as const,
};

/** The list the whole file reads: the cleared assignee filter, so the project document's own
 *  question - which nobody holds, a document having no assignee - is listed beside the rest. */
export const INBOX = "/?assignee=everyone";

export { inboxAt } from "./inbox-pane-scroll";

// ---------------------------------------------------------------------------
// The first spec: one section of 36 lines above the decision block.
// ---------------------------------------------------------------------------

export const SECTION_HEADING = "Rollout order";
export const DECISION_BLOCK = "rollout-order";
export const DECISION_QUESTION = "Which service moves first?";
const sectionLines = Array.from(
  { length: 36 },
  (_, index) =>
    `Rollout note ${index + 1}: the ledger and the mirror each keep a clock of their own.`
);
const decisionSpec = [
  `# ${SECTION_HEADING}`,
  "",
  ...sectionLines.flatMap((line) => [line, ""]),
  `:::ask{#${DECISION_BLOCK} urgency="med" multiple="false" state="open"}`,
  DECISION_QUESTION,
  "",
  "- Ledger first",
  "- Mirror first",
  ":::",
  "",
].join("\n");

// ---------------------------------------------------------------------------
// The second spec: versions 3 and 4, 22 blocks apart, approved at 3.
// ---------------------------------------------------------------------------

export const APPROVAL_HEADING = "Ledger rollout";
export const PLACEHOLDER = "Placeholder for the rollout body.";
/** The two paragraphs both versions hold, which the removed block sat between. */
export const SCOPE_ANCHOR = "Scope anchor: the ledger keeps a clock of its own in both versions.";
export const WINDOW_ANCHOR = "Window anchor: the mirror keeps its retry budget in both versions.";
/** The block version 4 no longer holds. */
export const RETIRED_BLOCK = "Retired note: the nightly mirror job runs at 02:00 every day.";
/** The one changed paragraph that kept all but one of its words. */
export const WORDS_BEFORE = "The retry budget is three attempts per lease.";
export const WORDS_AFTER = "The retry budget is seven attempts per lease.";
export const REMOVED_WORD = "three";
export const ADDED_WORD = "seven";
/** How many blocks version 4 changed since version 3. */
export const CHANGED_BLOCKS = 22;
/** Which of them is the one that changed only some of its words. */
export const WORD_LEVEL_STEP = 10;
const changedParagraphs = Array.from({ length: CHANGED_BLOCKS }, (_, index) =>
  index === WORD_LEVEL_STEP
    ? { after: WORDS_AFTER, before: WORDS_BEFORE }
    : {
        after: `Step ${index + 1}: the mirror reads the clock before the ledger writes it.`,
        before: `Step ${index + 1}: the ledger writes the clock before the mirror reads it.`,
      }
);
/** Version 3's body, inserted under the heading in one edit. */
const baselineBody = `${[
  SCOPE_ANCHOR,
  RETIRED_BLOCK,
  WINDOW_ANCHOR,
  ...changedParagraphs.map(({ before }) => before),
].join("\n\n")}\n`;

/** The four classes `features/doc/change-marks.ts` draws an approval's marks with. */
export const CHANGE_CLASSES = [
  "dispatch-change-block",
  "dispatch-change-added",
  "dispatch-change-removed",
  "dispatch-change-removed-words",
];

// ---------------------------------------------------------------------------
// The project document's own decision, and the two questions that are cards.
// ---------------------------------------------------------------------------

export const DOCUMENT_BLOCK = "mirror-window";
const documentContent = [
  "# Mirror runbook",
  "",
  "The mirror replays what the ledger wrote while it was away.",
  "",
  `:::ask{#${DOCUMENT_BLOCK} urgency="med" multiple="false" state="open"}`,
  "Which window does the mirror replay?",
  "",
  "- The last hour",
  "- The last day",
  ":::",
  "",
].join("\n");

export const ANCHOR_QUOTE = "replays the ledger";
export const ANCHORED_QUESTION = "Does a cold start replay the whole ledger?";
export const PLAIN_QUESTION = "Should the mirror keep retrying after a refused lease?";

export interface Seed {
  anchoredAskId: string;
  approvalArtifactId: string;
  approvalAskId: string;
  decisionAskId: string;
  decisionIssueKey: string;
  documentAskId: string;
  firstApprovalArtifactId: string;
  firstApprovalAskId: string;
  plainAskId: string;
}

/** The block index lands about 150 ms after the write that asks for it (measured), which falls
 *  inside Playwright's own 100/250/500 ladder: that ladder would not look again until a
 *  quarter-second after the row was already there, and every row of this file pays the seed. */
const INDEX_PROBE = { intervals: [50, 50, 100, 200, 500] };

/** The Inbox row `match` names, once the server has indexed the block or opened the question. */
async function indexedRow(match: (row: InboxRow) => boolean): Promise<InboxRow> {
  let found: InboxRow | undefined;
  await expect
    .poll(async () => {
      found = (await getInbox()).find(match);
      return found !== undefined;
    }, INDEX_PROBE)
    .toBe(true);
  if (found === undefined) {
    throw new Error("the seeded question left the Inbox before it was read");
  }
  return found;
}

/**
 * Six made-up questions, in the order the Inbox lists them (the issue's priority first): the
 * decision block of a spec, the request to approve version 4 of a spec approved at version 3, a
 * first request to approve another document, an anchored question, a question in no document, and
 * the decision block of a project document, which no issue priority orders.
 */
export async function seedInbox(): Promise<Seed> {
  await createProject({ key: "PANE", name: "Pane rollout" });

  // Every question is its own issue or document, and the Inbox orders them by the priorities set
  // here rather than by the order they were made, so each chain goes out beside the others
  // instead of after them. Only the two that share priority 3 stay in order, and they are made in
  // one chain of their own, since that tie is broken by which was made first.
  const [decision, approval, firstApproval, { anchored, plain }, documentDecision] =
    await Promise.all([
      (async () => {
        const issue = await createIssue({
          project: "PANE",
          spec: decisionSpec,
          title: "Rollout order",
        });
        await patchIssue(issue.key, { priority: 0 });
        const row = await indexedRow((indexed) => indexed.block_id === DECISION_BLOCK);
        return { issueKey: issue.key, row };
      })(),
      (async () => {
        const issue = await createIssue({
          project: "PANE",
          spec: `# ${APPROVAL_HEADING}\n\n${PLACEHOLDER}\n`,
          title: "Ledger rollout",
        });
        await patchIssue(issue.key, { priority: 1 });
        const artifactId = issue.primary_artifact_id;
        await editArtifact(
          artifactId,
          {
            ops: [{ after: "end", markdown: baselineBody, op: "insert" }],
            summary: "The rollout body",
          },
          session
        );
        await editArtifact(
          artifactId,
          { ops: [{ find: PLACEHOLDER, op: "delete" }], summary: "Drop the placeholder" },
          session
        );
        const atThree = withOpenedAsk(
          await requestApproval(artifactId, { summary: "The ledger-first rollout." }, session)
        );
        expect(atThree.version).toBe(3);
        await answerAsk(
          atThree.ask.id,
          { expected_edited_at: null, selected: ["Approve"] },
          { login: "alice" }
        );
        await editArtifact(
          artifactId,
          {
            ops: [
              { find: RETIRED_BLOCK, op: "delete" as const },
              ...changedParagraphs.map(({ after, before }) => ({
                find: before,
                op: "replace" as const,
                with: after,
              })),
            ],
            summary: "The mirror-first rollout",
          },
          session
        );
        const atFour = withOpenedAsk(
          await requestApproval(artifactId, { summary: "The mirror-first rollout." }, session)
        );
        expect(atFour.version).toBe(4);
        return { artifactId, askId: atFour.ask.id };
      })(),
      (async () => {
        const issue = await createIssue({
          project: "PANE",
          spec: "# Lease handover\n\nThe lease moves with the ledger, never with the mirror.\n",
          title: "Lease handover",
        });
        await patchIssue(issue.key, { priority: 2 });
        return {
          ...withOpenedAsk(
            await requestApproval(
              issue.primary_artifact_id,
              { summary: "The first handover draft." },
              session
            )
          ),
          artifactId: issue.primary_artifact_id,
        };
      })(),
      (async () => {
        const anchoredIssue = await createIssue({
          project: "PANE",
          spec: "The mirror replays the ledger on a cold start.\n",
          title: "Cold start",
        });
        await patchIssue(anchoredIssue.key, { priority: 3 });
        const anchored = await createAsk(
          anchoredIssue.key,
          { anchor: { artifact: "spec", quote: ANCHOR_QUOTE }, question: ANCHORED_QUESTION },
          session
        );
        const plainIssue = await createIssue({ project: "PANE", title: "Retry budget" });
        await patchIssue(plainIssue.key, { priority: 3 });
        const plain = await createAsk(
          plainIssue.key,
          { options: [{ label: "Yes" }, { label: "No" }], question: PLAIN_QUESTION },
          session
        );
        return { anchored, plain };
      })(),
      (async () => {
        await createProjectDocument("PANE", {
          content: documentContent,
          name: "mirror-runbook.md",
        });
        return indexedRow((indexed) => indexed.block_id === DOCUMENT_BLOCK);
      })(),
    ]);
  expect(firstApproval.version).toBe(1);

  return {
    anchoredAskId: anchored.id,
    approvalArtifactId: approval.artifactId,
    approvalAskId: approval.askId,
    decisionAskId: decision.row.id,
    decisionIssueKey: decision.issueKey,
    documentAskId: documentDecision.id,
    firstApprovalArtifactId: firstApproval.artifactId,
    firstApprovalAskId: firstApproval.ask.id,
    plainAskId: plain.id,
  };
}

// ---------------------------------------------------------------------------
// Open page: one issue with a spec and a second document, a project document, and a question
// of each kind the column draws.
// ---------------------------------------------------------------------------

export const OPEN_PROJECT = "OPEN";
/** The heading of each document, which neither of the other two holds. */
export const OPEN_SPEC_HEADING = "Spec heading only here";
export const OPEN_TALK_HEADING = "Talk heading only here";
export const OPEN_RUNBOOK_HEADING = "Runbook heading";
/** The decision blocks, each id once across the three documents, since `indexedRow` matches a
 *  row by its block id alone. */
export const OPEN_TALK_DECISION = "open-talk-decision";
export const OPEN_BEFORE_HEADING = "open-talk-before-heading";
const OPEN_SPEC_DECISION = "open-spec-decision";
const OPEN_RUNBOOK_DECISION = "open-runbook-decision";
const OPEN_ANSWERED = "open-talk-answered";
const ANSWERED_OPTIONS = ["The host", "The guest"] as const;

/** A decision block as this file's documents write one, with the blank line after it. */
function decisionBlock(id: string, question: string, options: readonly string[]): string[] {
  return [
    `:::ask{#${id} urgency="med" multiple="false" state="open"}`,
    question,
    "",
    ...options.map((option) => `- ${option}`),
    ":::",
    "",
  ];
}

const openSpec = [
  `# ${OPEN_SPEC_HEADING}`,
  "",
  "The spec says what the talk covers.",
  "",
  ...decisionBlock(OPEN_SPEC_DECISION, "Which part of the talk comes first?", [
    "The demo",
    "The slides",
  ]),
].join("\n");

/** `talk.md`: a decision block above any heading, then the heading, the paragraph question 4
 *  quotes, and two decision blocks under it. */
const talkContent = [
  "The talk runs for forty minutes.",
  "",
  ...decisionBlock(OPEN_BEFORE_HEADING, "Does the talk need a rehearsal?", ["Yes", "No"]),
  `# ${OPEN_TALK_HEADING}`,
  "",
  "The talk paragraph that a quote points at.",
  "",
  ...decisionBlock(OPEN_TALK_DECISION, "Which room holds the talk?", [
    "The small room",
    "The hall",
  ]),
  ...decisionBlock(OPEN_ANSWERED, "Who closes the talk?", ANSWERED_OPTIONS),
].join("\n");

const runbookContent = [
  `# ${OPEN_RUNBOOK_HEADING}`,
  "",
  "The runbook says how the room is set up.",
  "",
  ...decisionBlock(OPEN_RUNBOOK_DECISION, "Which step of the set-up runs first?", [
    "The lights",
    "The sound",
  ]),
].join("\n");

/** The questions of `seedOpenPage`, each by the Inbox row it is listed as and numbered as the
 *  row walks them, and what their addresses are built from. */
export interface OpenPageSeed {
  /** 1: a decision block under the heading of the issue's second document. */
  talkDecision: string;
  /** 2: a decision block under the heading of the issue's spec. */
  specDecision: string;
  /** 3: the request to approve the second document, and the version it names. */
  approval: string;
  approvalVersion: number;
  /** 4: a question on a quote of the second document, and the block the quote sits in. */
  quoted: string;
  quotedBlockId: string;
  /** 5: a question in no document. */
  plain: string;
  /** 6: a decision block under the heading of a project document, handed to alice. */
  runbookDecision: string;
  /** 7: a decision block of the second document that no heading stands above. */
  beforeHeading: string;
  /** 8: the answered decision block under the second document's heading, by its ask id, and the
   *  Reply thread row its answer and the session's reply make (`reply:<ask id>`). */
  answeredAskId: string;
  answeredThread: string;
  /** The block id of each document's heading. */
  headings: { runbook: string; spec: string; talk: string };
  issueKey: string;
  runbookSlug: string;
  talkSlug: string;
}

/**
 * One question of each kind an issue holds, a project document's decision block, and the Reply
 * thread of an answered decision block, all waiting on alice, who makes the project and the issue
 * and so is their assignee. The order is the point: answering question 8 writes its block's state
 * and saves a new version of `talk.md`, and a request to approve keeps the person's turn only
 * while no write has moved the document past the version it names, so every write to `talk.md`
 * comes before question 3.
 */
export async function seedOpenPage(): Promise<OpenPageSeed> {
  await createProject({ key: OPEN_PROJECT, name: "Open page" });
  const issue = await createIssue({ project: OPEN_PROJECT, spec: openSpec, title: "Talk week" });
  const { artifact: talk } = await createIssueArtifact(issue.key, {
    content: talkContent,
    name: "talk.md",
  });
  const { artifact: runbook } = await createProjectDocument(OPEN_PROJECT, {
    content: runbookContent,
    name: "runbook.md",
  });
  const [talkDecision, specDecision, runbookDecision, beforeHeading, answered] = await Promise.all([
    indexedRow((row) => row.block_id === OPEN_TALK_DECISION),
    indexedRow((row) => row.block_id === OPEN_SPEC_DECISION),
    indexedRow((row) => row.block_id === OPEN_RUNBOOK_DECISION),
    indexedRow((row) => row.block_id === OPEN_BEFORE_HEADING),
    indexedRow((row) => row.block_id === OPEN_ANSWERED),
  ]);

  // Question 8's answer puts `state="answered"` on its block's opening line, and the document's
  // settlement then writes the version that stores it. The live text serves no server-owned
  // attribute, so the newest version's stored markdown is where the state shows.
  // kept for one release: stored-ask-state
  await answerAsk(
    answered.id,
    { expected_edited_at: null, selected: [ANSWERED_OPTIONS[0]] },
    { login: "alice" }
  );
  await expect
    .poll(async () => {
      const newest = Math.max(
        ...((await getArtifact(talk.id)).versions ?? []).map((v) => v.number)
      );
      const { markdown } = await getArtifactVersion(talk.id, newest);
      return markdown.split("\n").find((line) => line.includes(`#${OPEN_ANSWERED}`)) ?? "";
    }, INDEX_PROBE)
    .toContain('state="answered"');

  const quoted = await createAsk(
    issue.key,
    {
      anchor: { artifact: talk.slug, quote: "a quote points at" },
      question: "Does every talk quote its sources?",
    },
    session
  );
  const quotedBlockId = quoted.anchor?.block_id;
  if (quotedBlockId === undefined || quotedBlockId === null) {
    throw new Error("question 4's quote names no block");
  }
  const plain = await createAsk(
    issue.key,
    { options: [{ label: "Yes" }, { label: "No" }], question: "Should the talk be recorded?" },
    session
  );
  await handAskOn(runbookDecision.id, "alice");

  // The session's reply in question 8's thread, after alice's answer: her Reply thread.
  await createComment(
    issue.key,
    { ask_id: answered.id, body: "The host closes it, then." },
    session
  );
  await expect
    .poll(
      async () => ((await listMyReplies()).rows ?? []).some((row) => row.thread_id === answered.id),
      { ...INDEX_PROBE, message: "item 8 is alice's Reply thread" }
    )
    .toBe(true);

  const approval = withOpenedAsk(
    await requestApproval(talk.id, { summary: "The talk as it stands." }, session)
  );

  const [talkHeading, specHeading, runbookHeading] = await Promise.all([
    blockId(talk.id, `# ${OPEN_TALK_HEADING}`),
    blockId(issue.primary_artifact_id, `# ${OPEN_SPEC_HEADING}`),
    blockId(runbook.id, `# ${OPEN_RUNBOOK_HEADING}`),
  ]);
  return {
    answeredAskId: answered.id,
    answeredThread: replyRowId(answered.id),
    approval: approval.ask.id,
    approvalVersion: approval.version,
    beforeHeading: beforeHeading.id,
    headings: { runbook: runbookHeading, spec: specHeading, talk: talkHeading },
    issueKey: issue.key,
    plain: plain.id,
    quoted: quoted.id,
    quotedBlockId,
    runbookDecision: runbookDecision.id,
    runbookSlug: runbook.slug,
    specDecision: specDecision.id,
    talkDecision: talkDecision.id,
    talkSlug: talk.slug,
  };
}

/** The reading column's two columns, as rendered boxes: the list and the pane beside it. */
export async function expectTwoColumns(page: Page): Promise<void> {
  const list = inboxList(page);
  const pane = inboxPane(page);
  await expect(list).toBeVisible();
  await expect(pane).toBeVisible();
  const [listBox, paneBox] = await Promise.all([list.boundingBox(), pane.boundingBox()]);
  if (listBox === null || paneBox === null) {
    throw new Error("the Inbox drew no box for one of its two columns");
  }
  expect(paneBox.x).toBeGreaterThanOrEqual(listBox.x + listBox.width);
}

/**
 * Where the pane's scrolled column, its drawn section heading and the block `blockId` stand, in
 * viewport pixels. The column is the scrolling ancestor of the drawn document inside `inbox-pane`:
 * the pane holds its head above it, so the top of the column is not the top of the pane.
 */
export function paneOffsets(page: Page, blockId: string) {
  return page.evaluate((id) => {
    const pane = document.querySelector<HTMLElement>('[data-testid="inbox-pane"]');
    if (pane === null) {
      throw new Error("the reading pane is not on the page");
    }
    const block = pane.querySelector<HTMLElement>(`[data-dispatch-ask-block="${CSS.escape(id)}"]`);
    const heading = pane.querySelector<HTMLElement>("h1");
    if (block === null || heading === null) {
      throw new Error("the pane has not drawn the document's heading and its decision block");
    }
    let column = pane;
    for (let node = heading.parentElement; node !== null && node !== pane; ) {
      const overflow = window.getComputedStyle(node).overflowY;
      if (overflow === "auto" || overflow === "scroll") {
        column = node;
        break;
      }
      node = node.parentElement;
    }
    const columnBox = column.getBoundingClientRect();
    return {
      block: block.getBoundingClientRect().top,
      columnBottom: columnBox.bottom,
      columnTop: columnBox.top,
      heading: heading.getBoundingClientRect().top,
    };
  }, blockId);
}
