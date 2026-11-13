import { createContext, type ReactNode, useCallback, useContext, useState } from "react";

import type { NodeType } from "./page-words";

/**
 * What the reader's focus is on, which the panel draws the facts of (dispatch://INFRA-1196 R12).
 *
 * The tree moves the focus: `IssueTree` calls `useSetFocusedNode` on every move, and hands back
 * `undefined` when the focus leaves the tree, which leaves the issue itself focused, so the
 * panel draws the facts of something rather than an empty box.
 */
export interface FocusedNode {
  /** Who made it, where the model holds an author: the author's own name. */
  readonly author?: string;
  /** The node's own address inside the issue (`[data-node-id]`). */
  readonly id: string;
  /** How it stands, as the page's own word for that state, where its type has one. */
  readonly state?: string;
  /** The node's own first line, or an issue's title: the author's words. */
  readonly title: string;
  readonly type: NodeType;
}

const FocusedNodeContext = createContext<FocusedNode | undefined>(undefined);

const SetFocusedNodeContext = createContext<(node: FocusedNode | undefined) => void>(() => {});

/**
 * Holds the focused node for one issue page, above the page itself: the tree sets it, and the
 * page reads it and hands it to the panel, whose facts are that node's, and to the preset row,
 * which draws the toolbar of the document holding it.
 */
export function FocusedNodeProvider({ children }: { children: ReactNode }): ReactNode {
  const [focused, setFocused] = useState<FocusedNode | undefined>(undefined);
  const set = useCallback((next: FocusedNode | undefined) => setFocused(next), []);
  return (
    <SetFocusedNodeContext.Provider value={set}>
      <FocusedNodeContext.Provider value={focused}>{children}</FocusedNodeContext.Provider>
    </SetFocusedNodeContext.Provider>
  );
}

/** What the focus is on, or `undefined` while nothing has taken it: the page then draws the
 *  issue's own facts, because a reader who is on no node is reading the issue. */
export function useFocusedNode(): FocusedNode | undefined {
  return useContext(FocusedNodeContext);
}

/** Moves the focus to `node`, or back to the issue itself with `undefined`. */
export function useSetFocusedNode(): (node: FocusedNode | undefined) => void {
  return useContext(SetFocusedNodeContext);
}
