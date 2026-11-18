import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  createElement,
  type ReactNode,
  useContext,
  useEffect,
  useMemo,
} from "react";
import { useLocation, useNavigate, useSearchParams } from "react-router-dom";

import { writeRequest } from "../../../api/client";
import { primarySpec } from "../../../api/issue-cache";
import { issueCoverageQuery } from "../../../api/queries";
import { refreshQueries } from "../../../api/query-refresh";
import type { IssueDetails } from "../../../api/types";
import { type IssueRoute, parseIssuePath, parseNodeReference } from "../../refs/routes";
import { useKeyedWrite } from "../../writes/useKeyedWrite";
import { type AddressTarget, issueAddress, pathToTarget, presetPath } from "../issue-address";
import { buildIssueTree, type TreeNode } from "../issue-tree";
import { CONTROL_LABELS } from "../page-words";
import { dropViews, issueTreeKey, issueTreeQuery } from "../tree-read/tree-read";
import { withLine } from "./node-states";

/**
 * What the page writes and reads around a node's life: the settle of a proposed node; the words
 * filter's words, by which the outline opens each line of overtaken nodes that holds a match; the
 * lines of overtaken nodes the page's address opens in a view; and what the tree read the page
 * already holds says of the overtaken nodes, for an address the Decisions view draws nowhere.
 */

/** `POST /api/v1/issues/{key}/nodes/{id}/settle`: a person settles the proposed node `reference`
 *  names, under the issue `key`. `{id}` is the node's whole address, scheme and all, which
 *  `routeUrl` percent-encodes as one path segment. It carries no body: a cookie proves a person,
 *  so the settle names no actor. */
function settleRequest(key: string, reference: string) {
  return writeRequest("post", "/api/v1/issues/{key}/nodes/{id}/settle", { id: reference, key });
}

/** The issue a node's own address names, which its settle goes under; undefined for
 *  an address that names none. The address says it with no context of the page's: Settle needs
 *  no writer, so it is drawn where no `OutlineWriterProvider` holds the issue's key. */
export function settleKeyOf(reference: string): string | undefined {
  const route = parseNodeReference(reference);
  return route !== undefined && "key" in route ? route.key : undefined;
}

/** The key of the issue the tree is drawn for, which the settle of a node whose own address names
 *  no issue goes under: the address of a lens, a risk, a constraint or a finding
 *  (`dispatch://lens/<uuid>`, `dispatch://lens/<uuid>#b-<block>`) names none, and the server
 *  settles such a node under the issue it stands under. Undefined outside a
 *  `SettleIssueProvider`. */
const SettleIssue = createContext<string | undefined>(undefined);

/**
 * Hands the key of the issue the tree is drawn for to each Settle under it (`SettleAct.tsx`), for
 * a node whose own address names no issue - a lens's, a risk's, a constraint's, a finding's -
 * which settles under the issue the tree is drawn for. `IssueTree` wraps its rows in it, with the
 * key of the issue its root is.
 */
export function SettleIssueProvider({
  children,
  value,
}: {
  children?: ReactNode;
  /** The key of the issue the tree is drawn for; undefined for a tree whose root is no issue. */
  value: string | undefined;
}): ReactNode {
  return createElement(SettleIssue.Provider, { value }, children);
}

/** The key of the issue the tree is drawn for, under which a node whose own address names no
 *  issue settles; undefined outside a `SettleIssueProvider`. */
export function useSettleIssue(): string | undefined {
  return useContext(SettleIssue);
}

/**
 * The settle of the proposed node `reference` names, under the issue `key`: one keyed write
 * per press, which the act that makes it shows refused itself (`inline`), so the shell's
 * indicator names it only while it is out, and the settles of one node go in press order. A 5xx
 * is made again under its key, so the write stays out while it is tried again.
 *
 * A landing - a 200, or `null` for `409 KEY_COMMITTED` - tells `landed` first, so the act puts
 * the focus on its row before anything is read again. It then reads the coverage again, reads
 * the tree again as the page does on an event of the issue (`refreshQueries`, which waits for a
 * read already out rather than cutting it off), and drops the view reads held for the load
 * (`dropViews`), so the open view is read again; the write stays out until the coverage's read
 * is answered. The tree and the views are those of the issue the tree is drawn for
 * (`SettleIssueProvider`), else of `key`: a node whose own address names another issue settles
 * under that issue, whose coverage is the one read again. The coverage is invalidated first and
 * the tree asked for after it, so the coverage's read begins first. The tree is asked for here,
 * not left to the tree read's subscription, which hears an invalidation of the coverage: a
 * coverage read already marked stale - a read of it failed, or a refresh still owes one - raises
 * none, and another issue's coverage is no query of the page's. While this write is out the
 * subscription holds the tree's read and the views this drops, and reads each once when the
 * write settles (`subscribeTreeFreshness` in `tree-read/tree-read.ts`).
 * A refusal reads nothing again.
 */
export function useSettle(key: string, reference: string, landed: () => void) {
  const queryClient = useQueryClient();
  const drawn = useSettleIssue() ?? key;
  return useKeyedWrite({
    inline: true,
    label: () => CONTROL_LABELS.settle,
    onSuccess: () => {
      landed();
      const coverage = queryClient.invalidateQueries({
        exact: true,
        queryKey: issueCoverageQuery(key).queryKey,
      });
      refreshQueries(queryClient, [{ exact: true, queryKey: issueTreeKey(drawn) }]);
      dropViews(queryClient, drawn);
      return coverage;
    },
    request: () => settleRequest(key, reference),
    target: () => `settle:${reference}`,
  });
}

/** Whether the words filter holds words, as the page's address carries them (`?q=`): the same
 *  value `issue-address.ts`'s `issueAddress` reads into `IssueAddress.filter` from the location's
 *  search, which `slots/body.tsx` narrows the outline's view by (`presets.ts`'s `presetView`),
 *  trimmed as that view trims it. False for an address that narrows nothing. */
export function useHoldsWords(): boolean {
  const [search] = useSearchParams();
  return (search.get("q") ?? "").trim() !== "";
}

/**
 * The lines of overtaken nodes the page's address opens in `root`, a view's own tree: the id of
 * each node whose line of overtaken children the path to the node the address names runs
 * through (`pathToTarget`, `withLine`), so that node is drawn as a row, as the outline opens its
 * own. The first node of the path at or under an overtaken one (`closed`) is drawn in the line
 * of the node above it, though in a view's tree it need not be overtaken itself - a row drawn
 * under the line of a parent the view draws no row for - so that line opens too. The address is
 * read as the slots read it (`issueAddress`); an address that opens a view opens no document, so
 * no spec slug is needed to read it. `target` says which node the address names, so a view opens
 * them once for each; `lines` is undefined for an address that names no node and until `root`
 * holds the one it names, and empty where the path runs through no closed node.
 */
export function useAddressLines(root: TreeNode): {
  readonly lines: ReadonlySet<string> | undefined;
  readonly target: string | undefined;
} {
  const { hash, pathname, search } = useLocation();
  return useMemo(() => {
    const route = parseIssuePath(pathname, search);
    const target =
      route === undefined ? undefined : issueAddress(route, search, hash, undefined).target;
    const path = pathToTarget(root, target);
    const lines = path === undefined ? undefined : new Set(withLine(new Set<string>(), path));
    const closed = path?.findIndex((node) => node.closed === true) ?? -1;
    const above = closed > 0 ? path?.[closed - 1] : undefined;
    if (above !== undefined) lines?.add(above.id);
    return {
      lines,
      target: target === undefined ? undefined : JSON.stringify(target),
    };
  }, [hash, pathname, root, search]);
}

/**
 * The outline's tree as the reads the page already holds build it (`buildIssueTree`): the tree
 * read (`issueTreeQuery`) of the issue `key`, with `issue`, so each node carries its state and
 * its reference as the outline's does. The read is watched by an observer that is never enabled,
 * so it sends no request and holds what the page read. Undefined until the tree read is
 * held, and while the caller hands no `issue`, which builds nothing.
 */
function useHeldTree(key: string, issue: IssueDetails | undefined): TreeNode | undefined {
  const tree = useQuery({ ...issueTreeQuery(key), enabled: false }).data;
  return useMemo(
    () => (issue === undefined || tree === undefined ? undefined : buildIssueTree({ issue, tree })),
    [issue, tree]
  );
}

/**
 * The address of an overtaken node, or of a node under one, under the Decisions view, which draws
 * such a node nowhere: it is replaced by the same address under no filter, its search and its
 * fragment kept, so the outline draws it, opens the line that holds it and focuses it, and the
 * stage, the filter row and the outline agree.
 * The address's target is read as the outline reads it (`issueAddress`, `pathToTarget`), in the
 * tree the page's held reads build (`useHeldTree`), so it costs no request; that tree is built
 * only for an address under Decisions that names a node, and nothing is replaced until the tree
 * read has answered. Every other address, under every other view, stays as it is.
 */
export function useOvertakenTarget(issue: IssueDetails, route: IssueRoute): void {
  const { hash, search } = useLocation();
  const navigate = useNavigate();
  const address = issueAddress(route, search, hash, primarySpec(issue)?.slug);
  const targeted = address.preset === "decisions" && address.target !== undefined;
  const root = useHeldTree(issue.key, targeted ? issue : undefined);
  // The target by its own fields, so a render that changes neither them nor the tree walks none.
  const target = address.target;
  const kind = target?.kind;
  const id = target?.id;
  const slug = target?.kind === "block" ? target.document : undefined;
  const closed = useMemo(() => {
    if (root === undefined || kind === undefined || id === undefined) return false;
    const named: AddressTarget = kind === "block" ? { document: slug, id, kind } : { id, kind };
    return pathToTarget(root, named)?.at(-1)?.closed === true;
  }, [id, kind, root, slug]);
  useEffect(() => {
    if (!closed) return;
    navigate({ hash, pathname: presetPath(issue.key, "conversation"), search }, { replace: true });
  }, [closed, hash, issue.key, navigate, search]);
}
