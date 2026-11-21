import { useQuery } from "@tanstack/react-query";
import { type FocusEvent, type ReactNode, useMemo, useState } from "react";

import { whoAmIQuery } from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { useAgentText } from "../refs/AgentName";
import { ChangeDraftsProvider } from "./DecisionActions";
import { artifactsBySlug, documentOfNode } from "./issue-artifacts";
import { NodeRow } from "./NodeRow";
import { threadMarks } from "./outline-thread-read";
import { authorKind, CONTROL_LABELS, isReader } from "./page-words";
import { treeRows } from "./tree-fold";
import { everythingOpened, rowKeyOf } from "./tree-keys";
import { useIssueTree } from "./useIssueTree";

/**
 * The conversation's rows, the first body of the new issue page (part G, R8; decision 4 of the
 * parts): every node under the
 * issue's own node of the tree (`useIssueTree.ts`), each drawn by `NodeRow`, every branch
 * unfolded at load, and no row for the issue, whose key and title the header draws.
 *
 * It is the outline's drawing without the outline: no fold to the frontier, no levels, no keys
 * and no one tab stop, which are part I's `IssueTree.tsx`, which replaces this file. So each row
 * here is a tab stop of its own; a press on a fold control folds its branch to one line with its
 * count and a second unfolds it; the focused row opens its body, and Keep open holds a body open
 * after the focus leaves. Every control a row draws is a tab stop too (`NodeRow`'s `tabStops`),
 * since no tree's keys reach it here (R9). A decision's Change keeps its form and the words typed
 * in it while its row's body is closed, and gives them back when the body opens again
 * (`ChangeDraftsProvider`). No row takes the focus at load, so the load scrolls nothing. Each
 * thread's newest write carries its read mark (`outline-thread-read.tsx`). Each row is handed what
 * `TreeRows.tsx` hands it.
 */
export function ConversationRows({ issue }: { issue: IssueDetails }): ReactNode {
  const { root } = useIssueTree(issue);
  // Each row's author by the page's one rule for naming one (`AgentText.author`), and the
  // reader, whose own words carry the ink rule and whose name is drawn in the ink (R3).
  const agentText = useAgentText();
  const reader = useQuery(whoAmIQuery()).data?.login;
  // The branches the reader folded shut; every other branch is drawn.
  const [closed, setClosed] = useState<ReadonlySet<string>>(() => new Set());
  const [focused, setFocused] = useState<string | undefined>(undefined);
  const [pinned, setPinned] = useState<ReadonlySet<string>>(() => new Set());
  const artifacts = useMemo(() => artifactsBySlug(issue), [issue]);
  const threads = useMemo(() => threadMarks(root), [root]);
  // Every branch opened, and below the drawn levels too, so no row stands for a deeper branch;
  // the issue's own row is the first and is left out, and every row under it moves up a level.
  const rows = useMemo(
    () =>
      treeRows(root, { ...everythingOpened(root), closed }).flatMap((row) =>
        row.kind === "node" && row.level > 1 ? [{ ...row, level: row.level - 1 }] : []
      ),
    [closed, root]
  );
  const isClosed = issue.closed_at !== null;
  const toggle = (id: string, expanded: boolean | undefined) => {
    if (expanded === undefined) return;
    setClosed((held) => {
      const next = new Set(held);
      if (expanded) next.add(id);
      else next.delete(id);
      return next;
    });
  };
  const pin = (key: string) =>
    setPinned((held) => {
      const next = new Set(held);
      if (!next.delete(key)) next.add(key);
      return next;
    });
  // The focus leaving every row closes the body it opened; a press inside the rows moves it to
  // another row, whose own focus opens that one's.
  const left = (event: FocusEvent<HTMLDivElement>) => {
    if (!event.currentTarget.contains(event.relatedTarget)) setFocused(undefined);
  };
  return (
    // Each row is a `treeitem` (`NodeRow`), so the rows stand in a tree, named as the outline's.
    <div
      aria-label={CONTROL_LABELS.tree}
      className="flex min-w-0 flex-col"
      data-conversation-rows=""
      onBlur={left}
      role="tree"
    >
      <ChangeDraftsProvider>
        {rows.map((row) => {
          const key = rowKeyOf(row);
          const place = documentOfNode(row.node.id, artifacts);
          const mark = threads.get(row.node.id);
          return (
            <NodeRow
              {...(row.node.answer === undefined
                ? {}
                : { answerer: agentText.author(row.node.answer.by) })}
              author={{
                kind: authorKind(row.node.author),
                name: agentText.author(row.node.author),
                reader: isReader(row.node.author, reader),
              }}
              body={focused === key || pinned.has(key)}
              context={false}
              {...(place === undefined ? {} : { document: place.artifact })}
              expanded={row.expanded}
              focused={focused === key}
              folded={row.folded}
              isClosed={isClosed}
              key={key}
              level={row.level}
              node={row.node}
              onFocus={() => setFocused(key)}
              onPin={() => pin(key)}
              onToggle={() => toggle(row.node.id, row.expanded)}
              ownAnswer={isReader(row.node.answer?.by, reader)}
              pinned={pinned.has(key)}
              rowKey={key}
              tabStop
              tabStops
              {...(mark === undefined ? {} : { threadRead: mark })}
            />
          );
        })}
      </ChangeDraftsProvider>
    </div>
  );
}
