import { useQuery } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";
import { useNavigate } from "react-router-dom";

import { whoAmIQuery } from "../../../api/queries";
import type { IssueDetails } from "../../../api/types";
import { buildIssuePath } from "../../refs/routes";
import { withThreadEntries } from "../contents-entries";
import type { TreeNode } from "../issue-tree";
import type { Preset } from "../page-words";
import { outlineOf } from "../tree-read/tree-read";
import { useTextThreads } from "./useTextThreads";

/**
 * What the Contents slot (`slots/contents.tsx`) adds for the reader's comments on text (R8, root
 * R14). The slot stands outside the stage, so outside the column's own answer: it builds the
 * threads of the outline read it already holds (`useTextThreads` watches the page's reads and
 * sends none), puts an entry for each thread the reader began in the tree it hands the list
 * (`withThreadEntries`), and sends a press on such an entry to the thread's address, as any
 * address of R8 opens it (`thread-address.ts`).
 */

/** What the slot passes the list besides what it passes it already: the tree with the entries in
 *  it. Optional in its type because it overrides the `root` the slot gave first. */
export interface ThreadEntryProps {
  readonly root?: TreeNode;
}

/** The entries the slot adds, and the way a press on one of them goes. */
export interface ThreadEntries {
  /** Opens the thread the entry `id` stands for, at its address, where `id` is such an entry:
   *  true. For every other entry it does nothing and answers false. */
  readonly go: (id: string) => boolean;
  readonly props: ThreadEntryProps;
}

/** The entries of the reader's comments on text of `issue` in `list`, the tree the Contents list
 *  is given (Conversation's view of `preset`'s outline read), and where a press on one goes. */
export function useThreadEntries(
  issue: IssueDetails,
  list: TreeNode,
  preset: Preset
): ThreadEntries {
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const view = useTextThreads(issue, outlineOf(preset) ?? "conversation");
  const key = issue.key;
  const { kinds, root } = useMemo(
    () => withThreadEntries(list, view.threads, viewer, key),
    [key, list, view.threads, viewer]
  );
  const navigate = useNavigate();
  const go = useCallback(
    (id: string): boolean => {
      const kind = kinds.get(id);
      if (kind === undefined) return false;
      navigate(buildIssuePath({ id, key, kind }));
      return true;
    },
    [key, kinds, navigate]
  );
  const props = useMemo(() => ({ root }), [root]);
  return { go, props };
}
