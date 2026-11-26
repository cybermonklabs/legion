import { useEffect, useMemo, useState } from "react";
import { flushSync } from "react-dom";

import { askForTask } from "./drawn-in-slices";
import type { TreeNode } from "./issue-tree";

/** The controls reserve room on their first drawing. Count in a later task instead of
 *  making that commit walk the tree. A held Verification view can build a tree when first
 *  read. Prepare that tree separately from counting, and publish the counts separately too. */
export function useCountsAfterDrawing<Counts>(
  root: TreeNode,
  count: (() => Counts) | undefined,
  immediate = false
): Counts | undefined {
  const [value, setValue] = useState<Counts | undefined>(undefined);
  const immediateValue = useMemo(
    () => (immediate && count !== undefined ? count() : undefined),
    [count, immediate]
  );
  useEffect(() => {
    if (immediate) return;
    if (count === undefined) {
      setValue(undefined);
      return;
    }
    let cancelCount: (() => void) | undefined;
    let cancelPublish: (() => void) | undefined;
    const compute = () => {
      const next = count();
      cancelPublish = askForTask(() => flushSync(() => setValue(() => next)));
    };
    const start = askForTask(
      root.views?.verification === undefined
        ? compute
        : () => {
            void root.views?.trees?.verification;
            cancelCount = askForTask(compute);
          }
    );
    return () => {
      start();
      cancelCount?.();
      cancelPublish?.();
    };
  }, [count, immediate, root]);
  if (immediate) return immediateValue;
  return count === undefined ? undefined : value;
}
