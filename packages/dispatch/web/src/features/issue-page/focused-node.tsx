import { createContext, type ReactNode, useCallback, useContext, useState } from "react";

import type { Actor, DisplayTree } from "../../api/types";
import type { TextSource } from "../refs/text-references";
import type { NodeType } from "./page-words";

/**
 * What the reader's focus is on, which the panel draws the facts of.
 *
 * The tree moves the focus: `IssueTree` calls `useSetFocusedNode` on every move, and hands back
 * `undefined` when the focus leaves the tree for anywhere but Details, which leaves the issue
 * itself focused, so the panel draws the facts of something rather than an empty box.
 */
export interface FocusedNode {
  /** Who made it, where the model holds an author: the panel names them by the page's one rule
   *  for an author (`AgentText.author`), so a name the registry learns later reaches it too. */
  readonly author?: Actor;
  /** The node's own address inside the issue (`[data-node-id]`). */
  readonly id: string;
  /** The ask, comment or message the node is, by its own id, which its pin and its thread's
   *  resolve are written to (`outline-writer.ts`'s `conversationItem`); absent for every other
   *  node. */
  readonly item?: { readonly id: string; readonly kind: "ask" | "comment" | "message" };
  /** The node's own `dispatch://` address (`TreeNode.reference`), where the model gives it one:
   *  what references it is read under that address, in Details
   *  (`node-references/NodeReferences.tsx`). */
  readonly reference?: string;
  /** The record the node's words are, whose issue reads each role in them (`TreeNode.source`). */
  readonly source?: TextSource;
  /** How it stands, as the page's own word for that state, where its type has one. */
  readonly state?: string;
  /** The node's own words as the tree holds them (`TreeNode.text`), or an issue's title: the
   *  author's words, which the panel names the node by as every place that names a node by its
   *  one line does - projected by the shared plain-text projection, each reference in them read
   *  as its target's current name (R8), and the page's word for the node's kind where they hold
   *  none (`node-line.tsx`'s `useNodeLine`, `lineOrKindWord`). They are handed whole rather than
   *  as a line cut from them, since only a draw can read a reference's name. */
  readonly text: string;
  /** The server's reading of `text` (`TreeNode.tree`), where the node has one. */
  readonly tree?: DisplayTree;
  readonly type: NodeType;
}

const FocusedNodeContext = createContext<FocusedNode | undefined>(undefined);

const SetFocusedNodeContext = createContext<(node: FocusedNode | undefined) => void>(() => {});

const LastFocusedIdContext = createContext<string | undefined>(undefined);

/**
 * Holds the focused node for one issue page, above the page itself: the tree sets it, and the
 * page reads it and hands it to the panel, whose facts are that node's, and to the preset row,
 * which draws the toolbar of the document holding it. Beside it, it holds the id of the node the
 * focus was last on, which the focus going back to the issue itself does not clear
 * (`useLastFocusedNodeId`).
 */
export function FocusedNodeProvider({ children }: { children: ReactNode }): ReactNode {
  const [focused, setFocused] = useState<FocusedNode | undefined>(undefined);
  const [lastId, setLastId] = useState<string | undefined>(undefined);
  // One setter sets both, so the node the focus is on is the last one in the same render, and
  // `undefined` leaves the last one as it was.
  const setFocusedNode = useCallback((node: FocusedNode | undefined) => {
    setFocused(node);
    if (node !== undefined) setLastId(node.id);
  }, []);
  return (
    <SetFocusedNodeContext.Provider value={setFocusedNode}>
      <FocusedNodeContext.Provider value={focused}>
        <LastFocusedIdContext.Provider value={lastId}>{children}</LastFocusedIdContext.Provider>
      </FocusedNodeContext.Provider>
    </SetFocusedNodeContext.Provider>
  );
}

/** What the focus is on, or `undefined` while nothing has taken it: the page then draws the
 *  issue's own facts, because a reader who is on no node is reading the issue. */
export function useFocusedNode(): FocusedNode | undefined {
  return useContext(FocusedNodeContext);
}

/** The id of the node the focus was last on, kept once the focus goes back to the issue itself,
 *  where `useFocusedNode` answers `undefined`, until it takes another node; `undefined` while no
 *  node has taken it. The provider is drawn anew for each issue (`slots/providers.tsx`), so it
 *  never names another issue's node. The Contents list reads it: the Outline sheet draws a new
 *  list each time it opens, after the focus left the outline for the Outline control, and that
 *  list saw no focus of its own (`IssueContents.tsx`). */
export function useLastFocusedNodeId(): string | undefined {
  return useContext(LastFocusedIdContext);
}

/** Moves the focus to `node`, or back to the issue itself with `undefined`. */
export function useSetFocusedNode(): (node: FocusedNode | undefined) => void {
  return useContext(SetFocusedNodeContext);
}
