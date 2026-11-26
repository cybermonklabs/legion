import { createContext, type ReactNode, useContext, useMemo } from "react";

import type { IssueDetails } from "../../api/types";
import { HeldChipLinesProvider } from "./held-chip-lines";
import { type IssueTreeView, useIssueTree } from "./useIssueTree";

/** The tree the page's owner holds (`IssueTreeOwnerProvider`), with the key of the issue it is
 *  the tree of. */
interface OwnedTree {
  readonly key: string;
  readonly tree: IssueTreeView;
}

/** The owner's tree, for every reader under it (`useOwnedIssueTree`); none where no owner stands
 *  above. */
const IssueTreeOwner = createContext<OwnedTree | undefined>(undefined);

/**
 * The owner of the issue's tree: the page's root mounts it once, around the slots it draws
 * (`IssueOutlinePage.tsx`), and it runs `useIssueTree` for the page, so every slot under it that
 * draws from the tree reads this one answer (`useOwnedIssueTree`). The view row, the Contents pane
 * and the outline each ran `useIssueTree` of their own on every render of a load of
 * Conversation, and each view's slot ran one again as a press mounted it; now only the owner runs
 * it, and a press on a view mounts no reader that builds the tree again. The owner reads the
 * address as each of those readers did (`useTreeRead`), so it holds the tree of the view the
 * address draws, Graph's read in Graph, and the reads, the names it publishes and the trees it
 * builds are those one reader made before.
 *
 * It holds for every chip under it the line the tree holds for each block it names, so a chip
 * holds its place before the names read names it (`held-chip-lines.tsx`).
 */
export function IssueTreeOwnerProvider({
  children,
  issue,
}: {
  readonly children: ReactNode;
  readonly issue: IssueDetails;
}): ReactNode {
  const tree = useIssueTree(issue);
  const { key } = issue;
  // One value for as long as the tree says the same, so a reader is drawn again only when it
  // changes.
  const owned = useMemo((): OwnedTree => ({ key, tree }), [key, tree]);
  return (
    <IssueTreeOwner.Provider value={owned}>
      <HeldChipLinesProvider namesFailed={tree.namesFailure !== undefined} root={tree.root}>
        {children}
      </HeldChipLinesProvider>
    </IssueTreeOwner.Provider>
  );
}

/**
 * The tree of `issue` for a reader of it (`IssueTreeView`): the owner's where an owner of that
 * issue's tree stands above (`IssueTreeOwnerProvider`), and else the reader's own
 * `useIssueTree(issue)`, as every reader built it before the owner. So a slot drawn with no owner
 * above it - a case that mounts one slot under the page's providers - reads, and sends, what it
 * did then, and two such readers still share one tree (`useIssueTree.ts`'s `builtTree`).
 */
export function useOwnedIssueTree(issue: IssueDetails): IssueTreeView {
  const owned = useContext(IssueTreeOwner);
  if (owned !== undefined && owned.key === issue.key) return owned.tree;
  // biome-ignore lint/correctness/useHookAtTopLevel: whether an owner of this issue's tree stands above is fixed for a mounted reader's life. The page mounts the owner once, with its slots, under the issue's key (`IssueOutlinePage.tsx`), and hands it and every slot the one issue, so a reader cannot move into or out of it, or to another issue under it, without mounting again.
  return useIssueTree(issue, true);
}
