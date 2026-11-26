import type {
  DecisionEntry,
  DecisionReading,
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
