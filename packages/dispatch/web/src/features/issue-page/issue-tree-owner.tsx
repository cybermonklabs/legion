import {
  createContext,
  type ReactNode,
  useContext,
  useLayoutEffect,
  useMemo,
  useState,
  useSyncExternalStore,
} from "react";

import type { IssueDetails } from "../../api/types";
import { createAgentTitleSelector } from "../refs/agent-registry";
import { AgentTitleScope } from "../refs/agent-title-scope";
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
  const [agentTitles] = useState(createAgentTitleSelector);
  return (
    <IssueTreeOwner.Provider value={owned}>
      <AgentTitleScope select={agentTitles}>
        <HeldChipLinesProvider namesFailed={tree.namesFailure !== undefined} root={tree.root}>
          {children}
        </HeldChipLinesProvider>
      </AgentTitleScope>
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
  const ownersTree = owned !== undefined && owned.key === issue.key ? owned.tree : undefined;
  // biome-ignore lint/correctness/useHookAtTopLevel: whether an owner of this issue's tree stands above is fixed for a mounted reader's life. The page mounts the owner once, with its slots, under the issue's key (`IssueOutlinePage.tsx`), and hands it and every slot the one issue, so a reader cannot move into or out of it, or to another issue under it, without mounting again.
  const tree = ownersTree ?? useIssueTree(issue, true);
  useTellBuilding(tree.building || tree.openingViewPending);
  return tree;
}

/**
 * Whether any reader of a tree under a stage still builds it from its latest reads
 * (`IssueTreeView.building`, `openingViewPending`). A tree is built in a task of its own after its
 * reads answer (`useIssueTree.ts`), so its rows are drawn a task or more after the reads settle,
 * and a stage that tells its view's first drawing once the reads settle waits on these builds too
 * (`slots/stage.tsx`'s `TellWhenSettled`). Every reader under the stage tells it, of the owner's
 * tree or its own (`useOwnedIssueTree`), so the stage builds no tree and sends no read. It is a
 * store outside React's state, so a build that starts or ends draws again only what watches it
 * (`useTreesBuilt`), and not the stage.
 */
export interface TreeBuilds {
  /** No reader under the stage builds its tree now. */
  readonly idle: () => boolean;
  /** `reader` builds its tree now, or no longer does. */
  readonly set: (reader: object, building: boolean) => void;
  readonly subscribe: (listener: () => void) => () => void;
}

/** A stage's builds (`TreeBuilds`), with no reader building. */
export function newTreeBuilds(): TreeBuilds {
  const building = new Set<object>();
  const listeners = new Set<() => void>();
  return {
    idle: () => building.size === 0,
    set: (reader, is) => {
      const wasIdle = building.size === 0;
      if (is) building.add(reader);
      else building.delete(reader);
      if (wasIdle !== (building.size === 0)) for (const listener of listeners) listener();
    },
    subscribe: (listener) => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
  };
}

/** The builds of the stage a reader stands under (`TreeBuilds`); none outside a stage. */
export const TreeBuildsContext = createContext<TreeBuilds | undefined>(undefined);

const noSubscribe = (): (() => void) => () => {};
const alwaysIdle = (): boolean => true;

/** Whether no reader under the stage still builds its tree (`TreeBuilds`); true outside a stage. */
export function useTreesBuilt(): boolean {
  const builds = useContext(TreeBuildsContext);
  return useSyncExternalStore(builds?.subscribe ?? noSubscribe, builds?.idle ?? alwaysIdle);
}

/** Tells the stage above, where one stands, whether this reader's tree is still being built. */
function useTellBuilding(building: boolean): void {
  const builds = useContext(TreeBuildsContext);
  const [reader] = useState(() => ({}));
  useLayoutEffect(() => {
    if (builds === undefined) return;
    builds.set(reader, building);
    return () => builds.set(reader, false);
  }, [builds, building, reader]);
}
