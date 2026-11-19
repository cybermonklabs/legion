import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

import type { IssueDetails } from "../../../api/types";
import { QueryError } from "../../../components/QueryError";
import { focusOnDocument } from "../../shell/roving";
import { bringOnScreen } from "../identity-block";
import { ANSWER_LINES, CONTROL_LABELS, type Preset } from "../page-words";
import { TAB_STOP_ROW } from "../tree-focus";
import {
  resetViews,
  subscribeTreeFreshness,
  TREE_FILTERS,
  type TreeFilter,
  useTreeRead,
  viewQuery,
} from "./tree-read";

/**
 * The filters' view reads of one load of the issue, around the stage. Each of Spec, Verification
 * and Plan is read the first time its filter is on in a load, and then held for the rest of it,
 * never stale by age. A filter's observer here watches its view only while the filter is on
 * (`ViewRead`), and `useIssueTree`'s, which never read, keep every view in the cache for the
 * load, so a filter turned off and on again finds its answer held and sends nothing; it finds a
 * refused view refused, whether it holds an answer or not, and sends nothing either, unless
 * something asked for that view again while the filter was off (`TREE_FRESH`): an event of the
 * issue, which reaches the views through its own target (`event-refresh.ts`). Once the stage
 * goes, the views are collected as any query nothing observes is.
 * `useIssueTree` watches the three reads without ever sending one, and sets what they hold on the
 * tree's root (`TreeNode.views`), which is what each filter draws from.
 *
 * The page mounts the stage once for each issue it opens (`IssueOutlinePage`'s `key`), so a mount
 * here is a load: it drops whatever view a past load of the issue held, so no load draws a view
 * older than its own tree. It holds the subscription that keeps the tree and its views fresh
 * (`subscribeTreeFreshness`), which holds them while a write of the issue is out: at the write's
 * settle, with the live feed up, only what the hold owes is asked for, and nothing for a write
 * Dispatch refused; with the feed down, every view is, so the open one is read again and draws
 * what it held until its new answer lands, and another is read again when its filter is next on.
 */
export function ViewReadProvider({
  children,
  issue,
  preset,
}: {
  children: ReactNode;
  issue: IssueDetails;
  preset: Preset;
}): ReactNode {
  const queryClient = useQueryClient();
  const key = issue.key;
  // Whether the views of a past load are dropped. No view is read before then: a view read as the
  // stage mounts would be dropped by that reset and read a second time.
  const [dropped, setDropped] = useState(false);
  useEffect(() => {
    resetViews(queryClient, key);
    setDropped(true);
  }, [key, queryClient]);
  useEffect(() => subscribeTreeFreshness(queryClient, key), [key, queryClient]);
  return (
    <>
      {dropped &&
        TREE_FILTERS.map((filter) => (
          <ViewRead filter={filter} issueKey={key} key={filter} on={preset === filter} />
        ))}
      {children}
    </>
  );
}

/**
 * One filter's view read, watched while the filter is on: its observer is subscribed then and
 * unsubscribed while the filter is off, rather than enabled and disabled. An observer enabled
 * again reads its view wherever the view is stale, a refused one included, since TanStack reads
 * a stale query then whatever its mount options say, so a refused view would be read on every
 * press of its filter. A subscribed one reads it as a new observer of the tree's reads does
 * (`TREE_FRESH`).
 */
function ViewRead({
  filter,
  issueKey,
  on,
}: {
  filter: TreeFilter;
  issueKey: string;
  on: boolean;
}): null {
  useQuery({ ...viewQuery(issueKey, filter), subscribed: on });
  return null;
}

/**
 * The outline's line for a tree read refused with no answer held, its retries spent: until the
 * read holds an answer the tree is the issue alone and the outline draws no row, so the line
 * says the read failed, with a Retry that reads the tree again, as each view's line says of a
 * read of its own. Nothing while the first read is out, and nothing once the read holds an
 * answer, which a failed refetch keeps.
 *
 * Once drawn, the line stays until a read answers (`useTreeRead`'s `failed`), whatever read is
 * out in between - the reader's Retry, an event's, the live feed's coming back, a focus's while
 * the feed is down - so the Retry stays on the page, and so does the keyboard's focus on it,
 * which would otherwise fall to the page's body. The Retry stays enabled, since a control
 * disabled while it holds the focus drops it, and a press while a read is out does nothing:
 * TanStack gives up a read in flight for a new one only where the query holds an answer, so the
 * press joins the read out and sends none. Refused again, the line stays as it was, the focus
 * with it. Answered, the line goes in the render that draws the outline's rows from the answer,
 * and a focus that was in it goes to the row that holds the tree's tab stop. Where the answer
 * draws no row - an issue that holds nothing yet - it goes to the outline's tree itself, which
 * takes it while it holds it, as the Plan view's empty tree does, and hands it on to the row
 * that holds the tab stop once one is drawn; never to the page's body.
 */
export function TreeReadFailed({ issue }: { issue: IssueDetails }): ReactNode {
  const { failed, refetch } = useTreeRead(issue);
  // The box the line stood in, which holds the outline's tree (`slots/body.tsx`), once the line
  // left the page with the focus in it, until a row of the tree takes that focus.
  const fellIn = useRef<HTMLElement | null>(null);
  // The outline's tree, while it holds that focus for want of a row (`holdFocus`).
  const holding = useRef<HTMLElement | null>(null);
  // One function for every render, so React runs its cleanup only when the line leaves the
  // page; it runs before the line is taken off the page, while the focus is still in it.
  const keepsFocus = useCallback((line: HTMLDivElement | null) => {
    if (line === null) return undefined;
    const box = line.parentElement;
    return () => {
      if (line.contains(document.activeElement)) fellIn.current = box;
    };
  }, []);
  // After every render, since the outline draws its rows in a render of the body, which draws
  // this line too. A focus that fell from the line goes to the tree's tab-stop row, else to the
  // tree itself, and from the tree to that row once one is drawn. A focus something else took in
  // the meantime stays there: anywhere but the page's body before the tree took it, anywhere but
  // the tree after.
  useLayoutEffect(() => {
    const box = fellIn.current;
    if (box === null) return;
    const held = holding.current;
    if (held === null ? !focusOnDocument() : document.activeElement !== held) {
      fellIn.current = null;
      holding.current = null;
      return;
    }
    const row = box.querySelector<HTMLElement>(TAB_STOP_ROW);
    if (row === null) {
      holding.current ??= holdFocus(box.querySelector<HTMLElement>(OUTLINE_TREE));
      return;
    }
    fellIn.current = null;
    holding.current = null;
    focusOn(row);
  });
  return failed ? (
    <div data-tree-unreadable="" ref={keepsFocus}>
      <QueryError message={ANSWER_LINES.outline_unreadable} onRetry={() => void refetch()} />
    </div>
  ) : null;
}

/** The outline's tree itself (`IssueTree.tsx`), by its name. */
const OUTLINE_TREE = `[role="tree"][aria-label="${CONTROL_LABELS.tree}"]`;

/** Puts the focus on `to` at once and without scrolling, and brings it on screen in the next
 *  frame (`bringOnScreen`) where it still holds the focus. */
function focusOn(to: HTMLElement): void {
  to.focus({ preventScroll: true });
  requestAnimationFrame(() => {
    if (document.activeElement === to) bringOnScreen(to);
  });
}

/** Puts the focus on the outline's tree, which takes none of its own (`IssueTree.tsx`): it takes
 *  it while it holds it, as the Plan view's empty tree does (`PlanFilterBody.tsx`), and no more
 *  once the focus leaves it, so Tab never stops on it beside its rows. A blur that puts the
 *  focus somewhere else - another control, or nothing - makes the tree take none from then on,
 *  at once. A blur that leaves the tree the focused element - the window going, as the reader
 *  switches to another, or a browser that moves the focus only after the blur - is no focus
 *  leaving it yet: the tree stays focusable, since a browser moves the focus to the body as soon
 *  as the focused element stops being focusable, and it looks again one task later, once every
 *  browser has finished the focus move the blur belongs to. Answers the tree, or null where none
 *  is drawn. */
function holdFocus(tree: HTMLElement | null): HTMLElement | null {
  if (tree === null) return null;
  if (!tree.hasAttribute("tabindex")) {
    tree.tabIndex = -1;
    // The focus has left the tree: it takes it no more, and no later blur needs a look.
    const letGo = () => {
      tree.removeAttribute("tabindex");
      tree.removeEventListener("blur", onBlur);
    };
    const onBlur = () => {
      if (document.activeElement !== tree) {
        letGo();
        return;
      }
      setTimeout(() => {
        if (document.activeElement !== tree) letGo();
      }, 0);
    };
    tree.addEventListener("blur", onBlur);
  }
  focusOn(tree);
  return tree;
}
