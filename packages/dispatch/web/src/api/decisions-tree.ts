import type {
  Decision,
  DecisionEntry,
  DecisionReading,
  IssueAskRow,
  IssueDecisions,
  IssueTreeNode,
  IssueTreeTuple,
} from "@legion/contracts";

import { compareTimestamps } from "../lib/timestamps";

type TreeEnd = IssueTreeTuple["from"];

function address(node: TreeEnd): string {
  return `${node.kind}:${node.id}`;
}

function resolutionOf(node: IssueTreeNode): DecisionEntry["resolution"] {
  const resolution = node.attributes?.resolution;
  if (resolution === undefined) return null;
  try {
    return JSON.parse(resolution) as DecisionEntry["resolution"];
  } catch {
    return null;
  }
}

/** Keep the screen's entry shape and order while reading the Decisions query's nodes. */
export function decisionsFromTree(
  issue: string,
  nodes: readonly IssueTreeNode[],
  tuples: readonly IssueTreeTuple[]
): IssueDecisions {
  const byAddress = new Map(nodes.map((node) => [address(node), node]));
  const deciders = new Map<string, TreeEnd>();
  const authors = new Map<string, TreeEnd>();
  const readings = new Map<string, DecisionReading[]>();
  for (const tuple of tuples) {
    if (tuple.kind === "decided_by") deciders.set(address(tuple.from), tuple.to);
    if (tuple.kind === "authored_by") authors.set(address(tuple.from), tuple.to);
  }
  for (const node of nodes) {
    if (node.role !== "query" || node.supertag !== "reading" || node.parent === null) continue;
    const parent = address(node.parent);
    const author = authors.get(address(node)) ?? node.author?.[0];
    const reading: DecisionReading = {
      artifact_id: node.document?.artifact_id ?? null,
      authored_by: author === undefined ? null : { id: author.id, kind: author.kind },
      block_id: node.block_id ?? "",
      node: { id: node.id, kind: node.kind },
      state: node.state,
      text: node.text,
    };
    const held = readings.get(parent);
    if (held === undefined) readings.set(parent, [reading]);
    else held.push(reading);
  }
  const ordered: { entry: DecisionEntry; madeAt: string | null }[] = [];
  for (const node of nodes) {
    if (node.role !== "query") continue;
    const { ask, decision, document } = node;
    if (ask === undefined && decision === undefined) continue;
    const resolution = ask === undefined ? null : resolutionOf(node);
    const declined = resolution?.kind === "declined";
    const ref = decision?.ref ?? `dispatch://${issue}/ask/${node.id}`;
    const block = node.block_id ?? decision?.block_id;
    const entryDocument =
      document === undefined || block === undefined || block === ""
        ? null
        : {
            artifact_id: document.artifact_id,
            block: node.id,
            block_id: block,
            ref: `dispatch://${issue}/${document.slug === "spec" ? "spec" : `artifact/${document.slug}`}`,
          };
    const decider = deciders.get(address(node));
    const title = decider === undefined ? undefined : byAddress.get(address(decider))?.title;
    ordered.push({
      madeAt: node.made_at,
      entry: {
        answer: ask?.answer ?? null,
        answer_tree: ask?.answer_tree,
        answer_state:
          ask === undefined
            ? null
            : (node.attributes?.answer_state ?? (decision ? "answered" : "open")),
        answered_at: ask?.answered_at ?? null,
        answered_by: ask?.answered_by ?? null,
        changed_at: decision?.changed_at ?? null,
        changed_version: decision?.changed_version ?? 0,
        decided_by: decider === undefined ? null : { ...decider, title },
        declined,
        declined_note: declined ? (resolution?.reason ?? null) : null,
        document: entryDocument,
        from_ask: ask === undefined ? (decision?.asks ?? []) : [],
        kept: decision?.kept ?? null,
        kept_by: decision?.kept?.kept_by ?? null,
        kept_text_sum: decision?.kept ? decision.text_sha256 : null,
        kind: ask === undefined ? "agent" : declined ? "declined" : "person",
        multiple: ask?.multiple ?? null,
        node: { id: node.id, kind: node.kind },
        options: ask?.options ?? null,
        question: ask === undefined ? null : node.text,
        readings: readings.get(address(node)) ?? [],
        ref,
        replaced: decision?.replaced ?? false,
        resolution,
        selected: ask?.selected ?? null,
        shaped: decision?.links ?? [],
        state: node.state,
        text: decision?.text ?? node.text,
        text_sha256: decision?.text_sha256 ?? "",
        text_tree: node.text_tree,
        uncertainty: decision?.uncertainty ?? null,
        uncertainty_reason: decision?.uncertainty_reason ?? null,
        urgency: ask?.urgency ?? null,
        waiting_review: decision?.waiting_review ?? false,
      },
    });
  }
  ordered.sort((a, b) => {
    const aOpen = a.entry.answer_state === "open";
    const bOpen = b.entry.answer_state === "open";
    if (aOpen !== bOpen) return aOpen ? -1 : 1;
    const at = ({ entry, madeAt }: (typeof ordered)[number]) =>
      entry.answered_at ?? entry.resolution?.at ?? entry.changed_at ?? madeAt;
    const left = at(a);
    const right = at(b);
    const time =
      left === null
        ? right === null
          ? 0
          : -1
        : right === null
          ? 1
          : compareTimestamps(left, right);
    if (time !== 0) return time;
    const first = address(a.entry.node);
    const second = address(b.entry.node);
    return first < second ? -1 : first > second ? 1 : 0;
  });
  return {
    decisions: [],
    document: null,
    entries: ordered.map(({ entry }) => entry),
    open_questions: [],
  };
}

/**
 * The Decisions read (`GET /api/v1/issues/{key}/decisions`) as a server of the release before
 * answers it: the open questions of the issue's spec and the sections of its decisions document,
 * and no `entries`.
 *
 * kept for one release: decisions-route
 */
export type ReleaseBeforeDecisions = Omit<IssueDecisions, "entries"> & {
  readonly entries?: IssueDecisions["entries"];
};

/**
 * The entries of the Decisions read of a server that serves no tree read, in the shape the tree's
 * Decisions view gives them (`decisionsFromTree`). A read that carries `entries` is handed back
 * with them. The release before's carries none, so its open questions are read as their asks'
 * entries, open, and its decisions as the entries they are, an agent's or, where a section cited
 * an ask, a person's, answered.
 *
 * kept for one release: decisions-route
 */
export function decisionsFromRead(issue: string, read: ReleaseBeforeDecisions): IssueDecisions {
  if (read.entries !== undefined) return { ...read, entries: read.entries };
  return {
    ...read,
    entries: [
      ...(read.open_questions ?? []).map((ask) => openQuestionEntry(issue, ask)),
      ...(read.decisions ?? []).map(sectionEntry),
    ],
  };
}

/**
 * An open question of the release before's Decisions read, as its ask's entry: the ask's own node
 * and address, its wording, and open. It names no document, since no view reads one of a
 * question, and no sum, since no Keep is of an ask.
 *
 * kept for one release: decisions-route
 */
function openQuestionEntry(issue: string, ask: IssueAskRow): DecisionEntry {
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
    ref: `dispatch://${ask.issue_key ?? issue}/ask/${ask.id}`,
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
 * A decision of the release before's Decisions read, one section of the issue's decisions
 * document, as its entry. Its address (`ref`) is its heading block's in that document, which
 * names the document and the block a Keep is written against, so it names no `document` of its
 * own: the read gives no id for it. A section that cites an ask is a person's decision, which an
 * entry names answered; the asks a section cites are the questions it settles (`from_ask`), and
 * its doubt is a `high` uncertainty. No entry of that read names a decider or a reading.
 *
 * kept for one release: decisions-route
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
