import { useQuery } from "@tanstack/react-query";
import { useEffect, useMemo } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { parseIssuePath } from "../../refs/routes";
import { type AddressTarget, presetPath } from "../issue-address";
import type { IssueTreeReads, TreeNode } from "../issue-tree";
import type { Preset } from "../page-words";
import { filterOfUndrawn } from "../presets";
import { pathTo } from "../tree-fold";
import { nextTreeCursor } from "../tree-read/tree-nodes";
import { outlineOf, outlineQuery, type TreeAnswer } from "../tree-read/tree-read";
import { type OnTextReads, onTextOf, onTextReads } from "./on-text";
import { threadHolds } from "./text-threads";
import { useStageIssue, useThreadReads, viewThreadsOf } from "./useTextThreads";

/**
 * Where an address that names an item of a thread on text opens (R8): `/comments/<id>`,
 * `?comment=<id>` and `/messages/<id>` of a comment or a message on text, or of a reply in its
 * thread, and `?node=<id>` of a lens a thread's comment carries (R7, where a lens invoked from a
 * thread lands). None of those items is a row of any view (R1), so the tree holds no node of them
 * for the address to land on; the node a thread stands by is the one to open, with its thread
 * beside it and the focus on the item (`TextMargin.tsx`).
 *
 * The order is decision 452's: Conversation where it draws the thread's node, else Graph, which
 * draws every node of its read. Where no view draws it - the thread stands in no margin - the
 * address opens the issue's own address with no line (decision 456), and in Threads once that
 * view is on the base (G8). The pages of the tree read, which a node on a later page waits on, are
 * read by the outline (`slots/body.tsx`'s `useReadUntilHeld`), as for any address that names a
 * node no loaded page holds, and this waits for them: a node found on a page that loads draws as
 * the first page's does (T's R15).
 */

/** What an address that names an item of a thread opens. */
export type ThreadTarget =
  /** The view draws the thread's node: the chain from the issue down to it, which the tree
   *  unfolds, draws the page of children of and puts the focus on. */
  | { readonly kind: "here"; readonly reveal: readonly TreeNode[] }
  /** Conversation draws no row of the node (a plan task with no step, a result): Graph does. */
  | { readonly kind: "graph" }
  /** No view draws the node: the issue's own address, with no line. */
  | { readonly kind: "none" };

/** What the target is decided from: the outline read the address's view draws (Conversation's,
 *  or Graph's in Graph), the page's reads, the tree built from them, and the view. */
interface ThreadTargetInput {
  readonly answer: TreeAnswer;
  readonly preset: Preset;
  readonly reads: IssueTreeReads;
  readonly root: TreeNode;
}

/** The kind of comment or message the item `id` is, as the reads hold it. */
function itemKindOf(
  id: string,
  { comments, messages, nodes }: OnTextReads
): "comment" | "message" | undefined {
  if (comments.has(id)) return "comment";
  if (messages.has(id)) return "message";
  const supertag = nodes.get(id)?.supertag;
  return supertag === "comment" || supertag === "message" ? supertag : undefined;
}

/**
 * Where the address that names `id` opens, or undefined where `id` names no item of a thread on
 * text, or the reads do not yet say (the page of comments or of the tree that would hold it is
 * not loaded): the address is then any other address of the page, resolved as before.
 */
export function threadTargetOf(id: string, input: ThreadTargetInput): ThreadTarget | undefined {
  const { answer, preset, reads, root } = input;
  for (const [host, { threads }] of viewThreadsOf(answer, reads).threads) {
    if (!threads.some((thread) => threadHolds(thread, id))) continue;
    const chain = pathTo(root, host);
    const node = chain?.at(-1);
    if (chain === undefined || node === undefined) return { kind: "none" };
    // Conversation draws no row of a plan task with no step or of a result (`filterOfUndrawn`).
    if (preset === "conversation" && filterOfUndrawn(node) !== undefined) return { kind: "graph" };
    return { kind: "here", reveal: chain };
  }
  // An item that is on text and stands in no margin: the read hangs it under no node a view draws
  // and its words are in none. The node may be on a page the tree read has not given yet.
  const onText = onTextReads(answer, reads);
  const supertag = itemKindOf(id, onText);
  if (supertag === undefined || onTextOf({ id, supertag }, onText) === null) return undefined;
  if (reads.comments === undefined || reads.messages === undefined) return undefined;
  return nextTreeCursor(answer) === undefined ? { kind: "none" } : undefined;
}

/** The address a thread target other than `here` opens, from the address it was named by: Graph's
 *  own, with the item as `?node=` (a message has no query of its own), or the issue's own
 *  address, `view`'s, less what named the item. The words filter and the fragment stay. */
function redirectOf(
  target: Exclude<ThreadTarget, { kind: "here" }>,
  id: string,
  at: {
    readonly hash: string;
    readonly key: string;
    readonly pathname: string;
    readonly search: string;
  },
  view: "conversation" | "graph"
): string {
  const query = new URLSearchParams(at.search);
  for (const name of ["ask", "comment", "node"]) query.delete(name);
  const route = parseIssuePath(at.pathname, at.search);
  const own = route?.kind === "issue" || route?.kind === "graph";
  let path = own ? at.pathname : presetPath(at.key, view);
  if (target.kind === "graph") {
    path = presetPath(at.key, "graph");
    query.set("node", id);
  }
  const rest = query.toString();
  return `${path}${rest === "" ? "" : `?${rest}`}${at.hash}`;
}

/**
 * The chain a view unfolds and lands on for an address `target` that names an item of a thread on
 * text, where the tree `root` holds no node `reveal` of it (`useAddressTarget`): undefined for
 * every other address, which resolves as before. Where Conversation draws no row of the thread's
 * node it opens Graph at the item, and where no view draws it the issue's own address without the
 * item, as `threadTargetOf` says; both replace the address, so Back leaves the page the reader
 * came from, and neither is drawn a line for. Reads nothing: it watches the reads the page holds,
 * and only while the issue is in the cache.
 */
export function useThreadReveal(
  root: TreeNode,
  target: AddressTarget | undefined,
  reveal: readonly TreeNode[] | undefined
): readonly TreeNode[] | undefined {
  const stage = useStageIssue(false);
  const { hash, pathname, search } = useLocation();
  const navigate = useNavigate();
  const preset = stage?.preset;
  const outline = preset === undefined ? undefined : outlineOf(preset);
  const key = stage?.issue.key ?? "";
  const answer = useQuery({ ...outlineQuery(key, outline ?? "conversation"), enabled: false }).data;
  const reads = useThreadReads(stage?.issue);
  const id =
    reveal === undefined && (target?.kind === "item" || target?.kind === "node")
      ? target.id
      : undefined;
  const found = useMemo(
    () =>
      id === undefined ||
      preset === undefined ||
      outline === undefined ||
      answer === undefined ||
      reads === undefined
        ? undefined
        : threadTargetOf(id, { answer, preset, reads, root }),
    [answer, id, outline, preset, reads, root]
  );
  const view =
    preset === "graph" ? "graph" : preset === "conversation" ? "conversation" : undefined;
  const to =
    id === undefined || found === undefined || found.kind === "here" || view === undefined
      ? undefined
      : redirectOf(found, id, { hash, key, pathname, search }, view);
  useEffect(() => {
    if (to !== undefined) navigate(to, { replace: true });
  }, [navigate, to]);
  return found?.kind === "here" ? found.reveal : undefined;
}
