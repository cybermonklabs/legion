import type { Decision, DecisionEntry, IssueAskRow, IssueDecisions } from "../../api/types";
import { buildDispatchReference } from "../refs/routes";

/**
 * The entries of the Decisions read (`GET /api/v1/issues/{key}/decisions`), which every view of
 * an issue's decisions reads: the Decisions tab, the issue page's tree, its Decisions filter and
 * its count of what waits, a decision's Keep and Change, the margin's decision cards and the
 * spec's stale marks. Each reads `entries` and nothing else of the read, so the page holds that
 * read under its one query key (`spec-marks.ts`'s `issueDecisionsQuery`) with its `entries`
 * always present (`withEntries`).
 *
 * This module is pure: it holds no React and makes no request.
 */

/**
 * The Decisions read as a server of either release answers it: this build's carries `entries`,
 * and a server of the release before answers no such member (`withEntries`).
 *
 * kept for one release: compat-objects
 */
export type DecisionsRead = Omit<IssueDecisions, "entries"> & {
  readonly entries?: IssueDecisions["entries"];
};

/** Whether an entry of the Decisions read is a decision: an agent's, or an ask a person answered.
 *  An open ask is a question, and a declined one decided nothing, so neither is a decision a view
 *  draws beside the nodes its `shaped` tuples reach, as the old `decisions` member never held
 *  them. */
export function isDecisionEntry(entry: Pick<DecisionEntry, "answer_state" | "kind">): boolean {
  return entry.kind === "agent" || entry.answer_state === "answered";
}

/**
 * `read` with its `entries`, as every view reads it. This build's server answers them, and the
 * read is handed back as it came. A server of the release before answers no `entries`, and a page
 * of this build can reach one while a deploy rolls: its `open_questions` and `decisions` are then
 * read as the entries they are, so no view says an issue has no decisions while that server
 * answered some. An open question is an open ask's entry; a decision is an entry of the decision
 * it is, an agent's or, where its section cited an ask, a person's, answered (`sectionEntry`).
 * `issueKey` is the issue the read is of, which an open question's address is built under where
 * its row names no issue.
 *
 * kept for one release: compat-objects
 */
export function withEntries(read: DecisionsRead, issueKey: string): IssueDecisions {
  if (read.entries !== undefined) return { ...read, entries: read.entries };
  return {
    ...read,
    entries: [
      ...(read.open_questions ?? []).map((ask) => openQuestionEntry(ask, issueKey)),
      ...(read.decisions ?? []).map(sectionEntry),
    ],
  };
}

/**
 * An open question of a read of the release before, as its ask's entry: the ask's own node and
 * address, its wording, and open. It names no document, since no view reads one of a question,
 * and no sum, since no Keep is of an ask.
 *
 * kept for one release: compat-objects
 */
function openQuestionEntry(ask: IssueAskRow, issueKey: string): DecisionEntry {
  return {
    answer: null,
    answer_state: "open",
    answered_at: null,
    answered_by: null,
    changed_at: null,
    changed_version: 0,
    decided_by: null,
    declined: false,
    declined_note: null,
    document: null,
    from_ask: [],
    kept: null,
    kept_by: null,
    kept_text_sum: null,
    kind: "person",
    multiple: ask.multiple,
    node: { id: ask.id, kind: "ask" },
    options: ask.options,
    question: ask.question,
    readings: [],
    ref: buildDispatchReference({ id: ask.id, key: ask.issue_key ?? issueKey, kind: "ask" }),
    replaced: false,
    resolution: null,
    selected: null,
    shaped: [],
    text: ask.question,
    text_sha256: "",
    ...(ask.question_tree === undefined ? {} : { text_tree: ask.question_tree }),
    uncertainty: null,
    uncertainty_reason: null,
    urgency: ask.urgency,
    waiting_review: false,
  };
}

/**
 * A decision of a read of the release before - one section of the issue's decisions document -
 * as its entry. Its address (`ref`) is its heading block's in that document, which names the
 * document and the block a Keep is written against (`decision-review.ts`'s `decisionDocumentId`
 * and `decisionBlockId`), so it names no `document` of its own: the read gives no id for it. A
 * section that cites an ask is a person's decision, which an entry names answered; the asks a
 * section cites are the questions it settles (`from_ask`), and its doubt is a `high`
 * uncertainty. No entry of that read names a decider or a reading.
 *
 * kept for one release: compat-objects
 */
function sectionEntry(decision: Decision): DecisionEntry {
  return {
    answer: null,
    answer_state: decision.kind === "person" ? "answered" : null,
    answered_at: null,
    answered_by: null,
    changed_at: decision.changed_at,
    changed_version: decision.changed_version,
    decided_by: null,
    declined: false,
    declined_note: null,
    document: null,
    from_ask: decision.asks ?? [],
    kept: decision.kept,
    kept_by: decision.kept?.kept_by ?? null,
    kept_text_sum: null,
    kind: decision.kind,
    multiple: null,
    node: { id: decision.ref, kind: "block" },
    options: null,
    question: null,
    readings: [],
    ref: decision.ref,
    replaced: decision.replaced,
    resolution: null,
    selected: null,
    shaped: decision.links ?? [],
    text: decision.text,
    text_sha256: decision.text_sha256,
    ...(decision.text_tree === undefined ? {} : { text_tree: decision.text_tree }),
    uncertainty: decision.unsure ? "high" : null,
    uncertainty_reason: null,
    urgency: null,
    waiting_review: decision.waiting_review,
  };
}
