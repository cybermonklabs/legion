import { type QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useRef, useSyncExternalStore } from "react";

import { api } from "../../../api/client";
import { conversationTreeKey, issueTreeKey } from "../../../api/queries";
import type { Artifact, IssueTreeRead } from "../../../api/types";
import { isBlockItemKind } from "../../refs/block-reference";
import { parseNodeReference } from "../../refs/routes";
import type { DocumentAdmissions } from "../document-admission";
import { documentOfNode } from "../issue-artifacts";
import type { TreeNode } from "../issue-tree";

/**
 * The room a node's text is edited in.
 *
 * An issue the conversion has reached is one room, and a node is edited through the fragment
 * that holds its text: its document's, or for a node written into the issue, such as a
 * criterion or a prose block, the fragment of the tree artifact the tree read's `tree` names. A
 * result's text is no fragment's, this issue's result or a borrowed one, so it takes no caret. A
 * node of another issue, such as a borrowed check, is read-only here and edited in its own
 * issue's room. On an issue the conversion has not reached, a node is edited in its document's
 * room, as `useNodeEditing.ts` does without this.
 *
 * `roomTarget` is pure. It answers the key of `KEY_REPORTS` a key reports, never its sentence.
 */

/** What the issue's tree read answers about its room: `converted`, `tree` and `viewOf` are the
 *  read's `converted`, `tree` and `view_of` for that issue, and `issueKey` is the issue the read
 *  is of. */
export interface RoomFacts {
  readonly converted: boolean;
  readonly issueKey: string;
  readonly tree: string | null;
  readonly viewOf: string | null;
}

/** Where the caret goes: the artifact a node's text is edited in and the node's block of it, or
 *  why it goes nowhere - `issue_above` for a node of another issue, `no_text` for a node whose
 *  text no fragment of this issue holds. */
export type RoomTarget =
  | { readonly artifactId: string; readonly blockId: string }
  | { readonly report: "issue_above" | "no_text" };

const ISSUE_ABOVE: RoomTarget = { report: "issue_above" };
const NO_TEXT: RoomTarget = { report: "no_text" };

/**
 * The room `node` is edited in, by its own address and the issue's documents (`artifacts`, by
 * slug, as `issue-artifacts.ts` holds them). On an issue the conversion has not reached, a node
 * is edited in the document `documentOfNode` finds.
 *
 * A result answers `no_text` before its address is read: no room holds a result's text, and a
 * borrowed result says `no_text`'s sentence rather than `issue_above`'s. Any other node whose
 * address names another issue answers `issue_above` before its id is read, since a borrowed
 * node's id can carry the slug of one of this issue's own documents. On a converted issue an ask,
 * a comment or a message takes no caret, an ask written as a block of a document too: its text
 * is the conversation's, which no fragment holds. A node of a document is edited in that
 * document's fragment. A criterion or a prose block no document holds and with no address was
 * written into the issue: the tree read names it by its block id in the tree artifact, bare or
 * after its place in the issue's tree (`<KEY>/tree#<block id>`), and gives it no address, so it is
 * edited in the tree artifact's own fragment at that block id; one whose id names another issue's
 * tree is that issue's. Any other node whose address names a block of this issue was written into
 * the issue too, and is edited there at that block.
 */
export function roomTarget(
  node: Pick<TreeNode, "id" | "reference" | "type">,
  artifacts: ReadonlyMap<string, Artifact>,
  room: RoomFacts
): RoomTarget {
  const place = documentOfNode(node.id, artifacts);
  const inDocument =
    place === undefined ? NO_TEXT : { artifactId: place.artifact.id, blockId: place.blockId };
  if (node.type === "result") return NO_TEXT;
  const route = node.reference === undefined ? undefined : parseNodeReference(node.reference);
  const ofIssue = route !== undefined && "key" in route ? route : undefined;
  if (ofIssue !== undefined && ofIssue.key !== room.issueKey) return ISSUE_ABOVE;
  if (!room.converted) return inDocument;
  if (isBlockItemKind(node.type) || isBlockItemKind(route?.kind)) return NO_TEXT;
  if (place !== undefined) return inDocument;
  if (room.tree === null) return NO_TEXT;
  if ((node.type === "criterion" || node.type === "prose") && node.reference === undefined) {
    const hash = node.id.indexOf("#");
    const owner = node.id.slice(0, Math.max(hash, 0));
    if (owner.includes("/") && !owner.startsWith(`${room.issueKey}/`)) return ISSUE_ABOVE;
    return { artifactId: room.tree, blockId: node.id.slice(hash + 1) };
  }
  const block = ofIssue !== undefined && "block" in ofIssue ? ofIssue.block : undefined;
  return block === undefined ? NO_TEXT : { artifactId: room.tree, blockId: block.id };
}

/**
 * Whether a row takes a caret: what Enter does on it, answered before Enter has opened anything.
 * It is the outline row's cue that it takes typing (`NodeRow`'s `editable`) and, in the
 * Verification and Decisions views, whether a press on a row's words puts the caret in. With no
 * room facts (`room` undefined) it answers what `useNodeEditing.ts`'s `nodeEditable` answers of
 * the document `documentOfNode` finds. In a room it answers by `roomTarget`: a node it names no
 * artifact for takes none - a node of another issue, a result, and on a converted issue a
 * question, a comment or a message, an ask block too - and a node it names one for takes a caret
 * on an open issue, unless that artifact's room admitted the page read-only. That is
 * `nodeEditable`'s rule read over the room's artifact id rather than a document's, written here
 * because `useNodeEditing.ts` imports this file, so the two agree.
 *
 * A node written into the issue - a criterion, a prose block of the tree - is why it exists: no
 * document holds one, so `nodeEditable` says it takes no typing, while Enter puts the caret in it
 * in the tree artifact's own fragment. That artifact is read only at the first Enter or press
 * (`useEditedTree`), so the rule reads the tree read's `tree` id and never an artifact.
 */
export function takesCaret(
  node: Pick<TreeNode, "id" | "reference" | "type">,
  artifacts: ReadonlyMap<string, Artifact>,
  room: RoomFacts | undefined,
  isClosed: boolean,
  admissions: DocumentAdmissions
): boolean {
  const target = room === undefined ? undefined : roomTarget(node, artifacts, room);
  // The artifact Enter edits the node in: the room's, or with no room the document
  // `documentOfNode` finds; none for a node it puts no caret in.
  const artifactId =
    target === undefined
      ? documentOfNode(node.id, artifacts)?.artifact.id
      : "artifactId" in target
        ? target.artifactId
        : undefined;
  // `nodeEditable`'s rule over that id: a closed issue takes no typing, nor does an artifact
  // whose room admitted the page read-only.
  return !isClosed && artifactId !== undefined && admissions.get(artifactId) !== true;
}

/**
 * The room facts of an issue's tree read, by the issue's key, read from the query cache with no
 * fetch of its own: from the outline read the page holds of the issue - Graph's, under the tree's
 * own key (`issueTreeKey`), else Conversation's (`conversationTreeKey`), the two keys
 * `tree-read/tree-read.ts`' `outlineQuery` reads under - else any view of the tree held under the
 * tree's key, each of which answers the issue's `converted`, `tree` and `view_of`. It answers
 * `undefined` for an issue whose tree read the cache does not hold, so its nodes are edited as
 * `useNodeEditing.ts` edits them without a room. It listens to the cache, so a tree read that
 * lands draws its caller again, and it answers the same object while the facts stay the same,
 * whatever else a new answer changes, so a caller may depend on it.
 */
export function useRoomFacts(issueKey: string | undefined): RoomFacts | undefined {
  const queryClient = useQueryClient();
  const held = useRef<RoomFacts | undefined>(undefined);
  const subscribe = useCallback(
    (changed: () => void) =>
      queryClient.getQueryCache().subscribe((event) => {
        if (event.type === "updated" || event.type === "removed") changed();
      }),
    [queryClient]
  );
  const snapshot = (): RoomFacts | undefined => {
    const answer = issueKey === undefined ? undefined : heldTreeRead(queryClient, issueKey);
    if (answer === undefined || issueKey === undefined) {
      held.current = undefined;
      return undefined;
    }
    const tree = answer.tree ?? null;
    const viewOf = answer.view_of ?? null;
    const last = held.current;
    if (
      last?.issueKey === issueKey &&
      last.converted === answer.converted &&
      last.tree === tree &&
      last.viewOf === viewOf
    ) {
      return last;
    }
    held.current = { converted: answer.converted, issueKey, tree, viewOf };
    return held.current;
  };
  return useSyncExternalStore(subscribe, snapshot, snapshot);
}

/** The tree read of issue `key` the cache holds an answer of, as `useRoomFacts` reads it: Graph's
 *  outline read, else Conversation's, else any view of the tree; undefined where it holds none.
 *  An answer is a tree read's where it says whether the issue converted. */
function heldTreeRead(queryClient: QueryClient, key: string): IssueTreeRead | undefined {
  const cache = queryClient.getQueryCache();
  for (const queryKey of [issueTreeKey(key), conversationTreeKey(key)]) {
    const answer = cache.find<IssueTreeRead>({ exact: true, queryKey })?.state.data;
    if (answer?.converted !== undefined) return answer;
  }
  for (const query of cache.findAll({ queryKey: issueTreeKey(key) })) {
    const answer = query.state.data as IssueTreeRead | undefined;
    if (answer?.converted !== undefined) return answer;
  }
  return undefined;
}

/**
 * The tree artifact a row is being edited in (`editing`, the artifact id `useOutlineEditing`
 * holds), where it is the one the tree read of the issue `issueKey` names as `tree`: a node
 * written into the issue, such as a criterion, is edited in it, so the body draws that
 * artifact's editor as it draws a document's. Its caller takes this only where no document the
 * issue read lists is `editing`, and that read never lists the tree artifact, so this asks the
 * tree read alone. The first Enter on such a row reads it with the artifact read
 * (`GET /api/v1/artifacts/{id}`) under the key the document's own editor reads an artifact by
 * (`ProofDocument.tsx`), so it is read once. `undefined` while the row edits anything else, or
 * nothing. While it reads no tree it does not subscribe to that key (`subscribed`), so a refetch
 * of the edited document's own read under the same key does not draw the body again.
 */
export function useEditedTree(issueKey: string, editing: string | undefined): Artifact | undefined {
  const tree = useRoomFacts(issueKey)?.tree;
  const reads = editing !== undefined && editing === tree;
  const read = useQuery({
    enabled: reads,
    queryFn: () => api.getArtifact(editing ?? ""),
    queryKey: ["artifact", editing],
    subscribed: reads,
  });
  return reads ? read.data : undefined;
}
