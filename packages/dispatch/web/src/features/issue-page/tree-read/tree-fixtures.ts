import type { Comment, Decision, IssueTreeEnd, IssueTreeParent } from "../../../api/types";
import type { TreeAnswer, TreeAnswerNode, TreeTuple } from "./tree-read";

/**
 * The tree read's answer as the unit tests build it, in the shape the route sends
 * (`IssueTreeRead`, generated from `model.IssueTreeRead`): a case names each node by its block id,
 * or by its own id where it is no block, and names its parent and a tuple's ends by those names;
 * `treeAnswer` gives each node the store kind and id the route gives it and names each parent and
 * each end by them, as the route does (`issue_tree_read.go`'s `node`, `parentOf` and `answer`).
 *
 * - **A block** - every typed block, a criterion, a decision, a kind no part draws - is the store's
 *   `block` at `<KEY>/<slug>#<block id>` (`treeIssueOf`), with its `block_id`: `<KEY>` is its
 *   `issue`, else this issue, and `<slug>` its `document`'s, else the one the case names (`in`),
 *   else the issue's own tree's.
 * - **Any other node** - the issue and its children, an ask, a comment, a message, a result, a
 *   person, a lens, a risk, a constraint - is of the kind of its supertag, by its own id, as the
 *   store keys it.
 *
 * A decision node carries the decisions read's object for it (`decision`), as the route answers
 * one: an agent's decision, never kept, replaced or changed, unless the case says otherwise. An
 * ask a person answered carries the person's decision that answer made where the case gives it
 * one, as the route answers it (`issue_tree_read.go`'s `readDecisions`): of kind `person`, the
 * ask's own; an ask nobody answered carries none.
 */

/** The issue every fixture of the page is read for. */
const TREE_KEY = "CORE-1";

/** The kinds the store keys a node by that are no block, each the supertag of its nodes. A typed
 *  block of another kind - an ask written in a document among them - is a `block`. */
const NODE_KINDS: readonly string[] = [
  "artifact",
  "ask",
  "comment",
  "component",
  "constraint",
  "interaction",
  "issue",
  "lens",
  "lens_kind",
  "message",
  "page",
  "person",
  "result",
  "risk",
  "role",
  "session",
];

/** What a case gives a node beside its name, its supertag and its parent: any member of the
 *  generated node but the ids `treeNode` gives it, with only the members of a decision's - or of
 *  an answered ask's - `decision` it says otherwise, and for a block no `document` places, the
 *  slug of the document it is written in (`in`). */
export type TreeNodeExtra = Partial<
  Omit<TreeAnswerNode, "block_id" | "decision" | "id" | "kind" | "parent">
> & {
  readonly decision?: Partial<Decision>;
  readonly in?: string;
};

/** The parent a case names by `name`, which `treeAnswer` names as the route does: a case that
 *  moves a node under another names its new parent so. */
export function treeParent(name: string): IssueTreeParent {
  return { first_line: "", id: name, kind: "", supertag: "" };
}

/**
 * One node of the answer, named `name`, of `supertag`, under the node `parent` names - a name
 * `treeAnswer` resolves, or the parent as the route names it where the answer does not hold it -
 * or under none. It is live, project-visible, made by no one recorded at one time and holds no
 * words, no rank and no attribute, as the case does not say otherwise.
 */
export function treeNode(
  name: string,
  supertag: string,
  parent: string | IssueTreeParent | null,
  extra: TreeNodeExtra = {}
): TreeAnswerNode {
  const { decision, in: slug, ...members } = extra;
  const node: TreeAnswerNode = {
    attributes: {},
    author: [],
    id: name,
    kind: supertag,
    made_at: "2026-11-05T00:00:00Z",
    parent: typeof parent === "string" ? treeParent(parent) : parent,
    rank: null,
    role: "tree",
    state: "live",
    supertag,
    text: "",
    visibility: "project",
    ...members,
  };
  const owner = node.issue ?? TREE_KEY;
  if (node.document === undefined && NODE_KINDS.includes(supertag)) {
    return supertag !== "ask" || decision === undefined
      ? node
      : {
          ...node,
          decision: decisionOf(node.text, {
            asks: [name],
            kind: "person",
            ref: `dispatch://${owner}/ask/${name}`,
            ...decision,
          }),
        };
  }
  const document = node.document?.slug ?? slug ?? "tree";
  return {
    ...node,
    block_id: name,
    ...(supertag === "decision"
      ? {
          decision: decisionOf(node.text, {
            block_id: name,
            kind: "agent",
            ref: `dispatch://${owner}/artifact/${document}#b-${name}`,
            ...decision,
          }),
        }
      : {}),
    id: `${owner}/${document}#${name}`,
    kind: "block",
  };
}

/** A comment as the tree read answers it: its words, who wrote it and when, under the node
 *  `parent` names - where none is given, the comment it replies to, else the issue. */
export function commentNode(
  comment: Pick<Comment, "author" | "body" | "created_at" | "id" | "reply_to">,
  parent: string = comment.reply_to ?? TREE_KEY
): TreeAnswerNode {
  return treeNode(comment.id, "comment", parent, {
    author: [comment.author],
    made_at: comment.created_at,
    text: comment.body,
  });
}

/** The decisions read's object for a decision whose words are `text`: never kept, replaced or
 *  changed, and naming no ask, no block and nothing it shaped, unless `members` says otherwise. */
function decisionOf(text: string, members: Partial<Decision>): Decision {
  return {
    asks: [],
    block_id: "",
    changed_at: null,
    changed_version: 1,
    heading: "",
    kept: null,
    kind: "agent",
    links: [],
    ref: "",
    replaced: false,
    text,
    text_sha256: "",
    unsure: false,
    waiting_review: false,
    ...members,
  };
}

/** A tuple of `kind` from the node `from` names to the node `to` names, which `treeAnswer`
 *  names by their store ends. */
export function treeTuple(kind: string, from: string, to: string): TreeTuple {
  return { from: { id: from, kind: "" }, kind, to: { id: to, kind: "" } };
}

/** The first line of `text` that says something, as the route cuts a parent's (`treeFirstLine`):
 *  past a typed block's opening line, its first line that is not blank, its marks trimmed. */
function firstLine(text: string): string {
  const lines = text.split("\n");
  if (lines[0]?.trim().startsWith(":::") === true) lines.shift();
  const line = lines.map((one) => one.trim()).find((one) => one !== "" && one !== ":::");
  return line === undefined ? "" : line.replace(/^[#>\-*+ ]+/u, "").trim();
}

/**
 * One answer of the tree read: `nodes` in the walk's order and `tuples` between them, each parent
 * and each end a case names by name named as the route names it - the nearest node before it of
 * that name, else the first the answer holds - with the parent's kind, supertag, first line and
 * issue. A name the answer holds no node of stays the id it names: an issue of that key, or a
 * block. The answer is of an issue that has not converted unless `extra` says otherwise.
 */
export function treeAnswer(
  nodes: readonly TreeAnswerNode[],
  tuples: readonly TreeTuple[] = [],
  extra: Partial<TreeAnswer> = {}
): TreeAnswer {
  // Each node by its name in the case - its block id, else its own id - and by its store id.
  const first = new Map<string, TreeAnswerNode>();
  for (const node of nodes) {
    for (const name of [node.block_id ?? node.id, node.id]) {
      if (!first.has(name)) first.set(name, node);
    }
  }
  const end = (named: IssueTreeEnd): IssueTreeEnd => {
    const node = first.get(named.id);
    if (node !== undefined) return { id: node.id, kind: node.kind };
    return named.kind === "" ? { id: named.id, kind: kindOfUnheld(named.id) } : named;
  };
  const latest = new Map<string, TreeAnswerNode>();
  const placed = nodes.map((node) => {
    const named = node.parent;
    let parent = named;
    if (named !== null && named.kind === "") {
      const held = latest.get(named.id) ?? first.get(named.id);
      parent =
        held === undefined
          ? { ...named, kind: kindOfUnheld(named.id), supertag: kindOfUnheld(named.id) }
          : {
              first_line: firstLine(held.text),
              id: held.id,
              kind: held.kind,
              supertag: held.supertag,
              ...(held.issue === undefined ? {} : { issue: held.issue }),
            };
    }
    for (const name of [node.block_id ?? node.id, node.id]) latest.set(name, node);
    return parent === named ? node : { ...node, parent };
  });
  return {
    converted: false,
    cursor: null,
    documents: null,
    issue: TREE_KEY,
    nodes: placed,
    tree: null,
    tuples: tuples.map((tuple) => ({ ...tuple, from: end(tuple.from), to: end(tuple.to) })),
    version: null,
    view: "",
    view_of: null,
    ...extra,
  };
}

/** A tree page with the cursor of the next page and, when given, the count after this page. */
export function pagedAnswer(
  page: TreeAnswer,
  cursor: string | null,
  remaining?: number
): TreeAnswer {
  return { ...page, cursor, ...(remaining === undefined ? {} : { remaining }) };
}

/** The tree read's stub for a case whose view is drawn off the whole tree, as before the page
 *  holds its view read (`presets.ts`'s `presetView`): the whole tree answers `answer`, and each
 *  view of it never answers. */
export function wholeTreeOnly(
  answer: TreeAnswer
): (key: string, query?: { readonly view?: string }) => Promise<TreeAnswer> {
  return (_key, query = {}) =>
    query.view === undefined ? Promise.resolve(answer) : Promise.withResolvers<never>().promise;
}

/** The kind of a node a case names that the answer does not hold: an issue, by its key, else a
 *  block. */
function kindOfUnheld(name: string): string {
  return /^[A-Z][A-Z0-9]*-\d+$/u.test(name) ? "issue" : "block";
}
