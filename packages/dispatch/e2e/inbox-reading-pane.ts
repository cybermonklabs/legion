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
  createIssue,
  createProject,
  createProjectDocument,
  editArtifact,
  getInbox,
  patchIssue,
  requestApproval,
  withOpenedAsk,
} from "./api";
import { inboxList, inboxPane } from "./inbox-pane";

export const session = {
  actor: { id: "e2e-reading-pane", kind: "session" as const },
  as: "agent" as const,
};

/** The list the whole file reads: the cleared assignee filter, so the project document's own
 *  question - which nobody holds, a document having no assignee - is listed beside the rest. */
export const INBOX = "/?assignee=everyone";

/** The address the Inbox writes for a selected row (`buildInboxPath`: the item, then the filter). */
export function inboxAt(askId: string): string {
  return `/?item=${askId}&assignee=everyone`;
}

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
        return withOpenedAsk(
          await requestApproval(
            issue.primary_artifact_id,
            { summary: "The first handover draft." },
            session
          )
        );
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
    firstApprovalAskId: firstApproval.ask.id,
    plainAskId: plain.id,
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
