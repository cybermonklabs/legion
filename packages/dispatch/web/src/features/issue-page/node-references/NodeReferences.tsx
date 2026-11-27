import { skipToken, useQuery } from "@tanstack/react-query";
import { type ReactNode, useId, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../../api/client";
import { referenceTargetsQuery } from "../../../api/queries";
import type { GraphReferences, ReferenceTarget } from "../../../api/types";
import { pageHeaderControl, pageInkText, pageQuietText } from "../../../theme/classes";
import { markdownClassName } from "../../refs/markdown-engine";
import { drawsEdge } from "../../refs/ReferencedBy";
import { useKnownReferences } from "../../refs/reference-names";
import { referencesQuery, standsFailed } from "../../refs/reference-target";
import { parseNodeReference, referenceHref } from "../../refs/routes";
import { DetailsErrorLine, retryRead, useRetryHandOff } from "../details-error";
import { useListPages } from "../details-leaves";
import type { FocusedNode } from "../focused-node";
import { useMarginDrawer } from "../margin-slot";
import { shortDay } from "../node-time";
import { authorText, pageChrome } from "../page-structure";
import { ANSWER_LINES, CONTROL_LABELS, NODE_REFERENCES_WORDS } from "../page-words";
import { ROW_LINK } from "../row-references";
import {
  citingBlockRefs,
  issueItemWords,
  type ReferenceRow,
  referenceRows,
} from "./references-list";

const EDGE_WORDS: Readonly<Record<string, string>> = NODE_REFERENCES_WORDS.edges;
const KIND_WORDS: Readonly<Record<string, string>> = NODE_REFERENCES_WORDS.kinds;

/** The words of no item, while the list is shut. */
const NO_ITEM_WORDS: ReadonlyMap<string, string> = new Map();

/** The rows of no list, while the list is shut. */
const NO_ROWS: readonly ReferenceRow[] = [];

/** The edges of no list, and the addresses and members of no names read: while the list is shut,
 *  or the read has not answered. */
const NO_EDGES: Readonly<NonNullable<GraphReferences["edges"]>> = [];
const NO_REFS: readonly string[] = [];
const NO_TARGETS: readonly ReferenceTarget[] = [];

/**
 * The words of the comments, messages and questions of the focused row's own issue, by their
 * ids (`issueItemWords`), while the list is open. They come from the reads the outline is drawn
 * from, under their own keys and with their own functions (`useIssueTree.ts`), watched and never
 * asked for, so they cost no request; while a read has no answer its items have no words
 * here. A row whose address names no issue has no items to read.
 */
function useIssueItemWords(
  issueKey: string | undefined,
  open: boolean
): ReadonlyMap<string, string> {
  const asks = useQuery({
    enabled: false,
    queryFn: issueKey === undefined ? skipToken : () => api.listIssueAsks(issueKey, "all"),
    queryKey: ["asks", issueKey],
  });
  const comments = useQuery({
    enabled: false,
    queryFn: issueKey === undefined ? skipToken : () => api.listComments(issueKey),
    queryKey: ["comments", issueKey],
  });
  const messages = useQuery({
    enabled: false,
    queryFn: issueKey === undefined ? skipToken : () => api.listIssueMessages(issueKey),
    queryKey: ["issue-messages", issueKey],
  });
  return useMemo(
    () =>
      open
        ? issueItemWords({ asks: asks.data, comments: comments.data, messages: messages.data })
        : NO_ITEM_WORDS,
    [asks.data, comments.data, messages.data, open]
  );
}

/**
 * What references the row the reader is on, in Details: the panel holds what references the
 * issue or the focused node, and a block's old 'Referenced by' list now stands in the focused
 * node's facts.
 *
 * One link per node that points at the focused one, mentions included, each landing on that
 * node: a node of this issue opens focused in the outline, and a node of another issue or of a
 * project document opens where that address opens (`referenceHref`). The outline
 * already carries two of the three edges the old disclosure listed - what serves a node is its
 * spine and what needs it is a chip - and nothing on the page points back from a node to what
 * mentions it, so this is where a reader reads that.
 *
 * Each row names the citing node by words the page holds for it: a comment, a message
 * or a question of this issue by all of its words, drawn whole with no cut and no clamp; any
 * other block - a task, a statement, a check, the block of an item of another issue - by the
 * first line of its own content, read and cut as the outline reads and cuts a row's one line,
 * from the names read of the citing blocks, which answers a block with the server's reading of
 * its content and never its opening line (decisions 560 and 565), so two tasks of one plan that
 * serve the row read apart; and a code block by the first line of its code, drawn as code. Each
 * is beside its word, and each reference in its words, a code block's apart, is named from the
 * page's one batched read of what references point at (`useKnownReferences`), as the outline's
 * row names it. A node with no words, and a block the names read has not answered for or
 * answered gone, is named by the word of its tie or of its kind alone, with its issue and its
 * time. The read's excerpt is the node's stored text, the markdown its author typed with every
 * address it cites, and is drawn nowhere. The rows are drawn 20 at a time, with Show more under
 * them (`ReferenceList`).
 *
 * It costs no request on the first load and none per node. Two gates hold that: the
 * reader opens the list, and Details is open around it, which on the issue page it is not when
 * the page loads. The list closes again when the focus moves to another row, so a reader
 * walking the outline with Details open reads one node's references and not every node's. The
 * read is `referencesQuery`, under the key every other Referenced-by panel reads
 * (`["references", "to", <address>]`), so a node read here and opened elsewhere is read once
 * and an event that changes it refreshes both (`api/event-refresh.ts`). Once it has answered, the
 * citing blocks' own addresses (`citingBlockRefs`) are one batched names read behind the same two
 * gates, `referenceTargetsQuery` under its own key, so a set the page has read costs nothing. The
 * items' words are the outline's reads, which this list never asks for (`useIssueItemWords`).
 */
export function NodeReferences({
  node,
  reference,
}: {
  /** The row the reader is on. The issue's own references are Details' References row, so the
   *  issue draws no list of its own here and nothing is drawn twice (c-one-panel). */
  node: Pick<FocusedNode, "id" | "type">;
  /** The focused node's own `dispatch://` address (`TreeNode.reference`); `undefined` for a
   *  node the model gives no address, which nothing can reference. */
  reference: string | undefined;
}): ReactNode {
  const drawer = useMarginDrawer();
  const panelId = useId();
  // Which row the reader opened the list on. The focus moving to another row closes it, and a
  // reader coming back to the row finds it closed: the row the list was opened on is let
  // go of as the focus leaves it, in the render that moves it.
  const [openedOn, setOpenedOn] = useState<string | undefined>(undefined);
  if (openedOn !== undefined && openedOn !== node.id) setOpenedOn(undefined);
  const addressed = node.type === "issue" ? undefined : reference;
  const open = addressed !== undefined && openedOn === node.id;
  // Outside the shell no drawer holds the panel and it is always open; on the issue page it is
  // the Details drawer, which opens shut.
  const reading = open && (drawer === undefined || drawer.open);
  const query = useQuery({ ...referencesQuery(addressed), enabled: reading });
  // The issue the focused row is of: a citing node of another issue, or of a project, says
  // which, as the old panel's "Comment · LAY-9" did; a citing item of this issue is named
  // by its words.
  const here = addressed === undefined ? undefined : parseNodeReference(addressed);
  const issueKey = here !== undefined && "key" in here ? here.key : undefined;
  const itemWords = useIssueItemWords(issueKey, open);
  // The names the page's one batched read gave what its references point at, and which of them
  // name a post its author edited, by which a reference in a citing item's line is named: a
  // context the page publishes, so no request.
  const { edited, names } = useKnownReferences();
  // An edge a newer build answers with, of a kind or to a node this page does not know, is left
  // out, as every list of a node's references leaves it (`drawsEdge`): its kind's word would be
  // the node kind's, which says it was only named. Built while the list is open, and again only
  // when what they are read from changes, not on every move of the focus.
  const edges = useMemo(
    () => (open ? (query.data?.edges ?? NO_EDGES).filter(drawsEdge) : NO_EDGES),
    [open, query.data]
  );
  // The citing blocks' own words: one batched names read of their addresses once the references
  // have answered, behind the references read's own gates.
  const lineRefs = useMemo(() => (open ? citingBlockRefs(edges) : NO_REFS), [edges, open]);
  const targets = useQuery({
    ...referenceTargetsQuery(lineRefs),
    enabled: reading && lineRefs.length > 0,
  });
  const rows = useMemo(
    () =>
      open
        ? referenceRows(
            itemWords,
            edges,
            targets.data?.targets ?? NO_TARGETS,
            { edited, names },
            issueKey
          )
        : NO_ROWS,
    [edges, edited, issueKey, itemWords, names, open, targets.data]
  );
  const toggle = useRef<HTMLButtonElement>(null);
  // Retry goes with its line once the read it sent lands, under the reader's own press, and the
  // focus would fall to the page with it: it goes back to the list's own control instead, as
  // Unpin in Pinned hands it on (`IssuePins.tsx`). Retry is never disabled while it reads again,
  // since Chromium drops the focus of a focused button that turns disabled to the page, so a read
  // that fails again leaves the focus on it; it reads busy then, and a press while the list is
  // read joins that read rather than ending it and sending another (`retryRead`).
  const retry = useRetryHandOff(() => toggle.current);
  // The failure line stands in place of the loading line while the read stands failed
  // (`standsFailed`).
  const failed = standsFailed(query);
  if (addressed === undefined) {
    return null;
  }
  return (
    <section
      aria-label={NODE_REFERENCES_WORDS.what_references}
      className="mt-2"
      data-node-references={node.id}
    >
      <button
        aria-controls={panelId}
        aria-expanded={open}
        className={pageHeaderControl}
        onClick={() => setOpenedOn(open ? undefined : node.id)}
        ref={toggle}
        type="button"
        {...pageChrome}
      >
        {NODE_REFERENCES_WORDS.what_references}
      </button>
      {open ? (
        <div id={panelId}>
          {reading && query.isPending && !failed ? (
            <p className={`mt-1 text-xs ${pageQuietText}`} {...pageChrome}>
              {ANSWER_LINES.references_loading}
            </p>
          ) : null}
          {failed ? (
            <div className="mt-1">
              <DetailsErrorLine
                error={query.error}
                failure={query.errorUpdateCount}
                message={ANSWER_LINES.references_unreadable}
                onRetry={retryRead(query)}
                {...retry}
                retrying={query.isFetching}
              />
            </div>
          ) : null}
          {query.data !== undefined && rows.length === 0 ? (
            <p className={`mt-1 text-xs ${pageQuietText}`} {...pageChrome}>
              {CONTROL_LABELS.none}
            </p>
          ) : null}
          {rows.length === 0 ? null : <ReferenceList issueKey={issueKey} rows={rows} />}
        </div>
      ) : null}
    </section>
  );
}

/**
 * The rows of an open list of what references the row the reader is on, 20 at a time with Show
 * more under them (`useListPages`). It is drawn only while the list is open and holds a row, so a
 * list opened again, on this row or on another, starts at its first 20 again. `issueKey` is the
 * issue of the row the reader is on, whose own citing nodes name no issue.
 */
function ReferenceList({
  issueKey,
  rows,
}: {
  issueKey: string | undefined;
  rows: readonly ReferenceRow[];
}): ReactNode {
  const pages = useListPages<HTMLUListElement>();
  return (
    <>
      <ul className="mt-1 flex min-w-0 flex-col gap-0.5" ref={pages.list}>
        {rows.slice(0, pages.drawn).map((row) => {
          const route = row.ref === undefined ? undefined : parseNodeReference(row.ref);
          const href = route === undefined ? undefined : referenceHref(route);
          // The edge's own word where the model gives the tuple one, and the kind of node where
          // it does not, which is every mention: the old panel's own rule.
          const word = EDGE_WORDS[row.kind] ?? KIND_WORDS[row.nodeKind];
          // A citing node is named by its words - a comment, a message or a question of this
          // issue by all of them, which wrap within the row, any other node by its first line,
          // a code block by its code's - beside its word; a node with no words, an item whose
          // read has not answered, and a block the names read has not named, by its word alone,
          // which is then the link: its stored text is markup, and an address, a slug or an id
          // is no name.
          const named = row.words !== "";
          const elsewhere = row.source !== undefined && row.source !== issueKey;
          const text = named ? row.words : (word ?? "");
          // A code block's line is drawn as code, in the look the renderer gives a code span in
          // a node's words (`MarkdownBody`'s inline variant: a `code` in `markdownClassName`),
          // its characters as typed: no address in it is named or linked.
          const drawn = row.code ? (
            <span className={markdownClassName}>
              <code>{text}</code>
            </span>
          ) : (
            text
          );
          return (
            <li className="flex min-w-0 items-baseline gap-2 text-sm" key={row.key}>
              {word === undefined || !named ? null : (
                <span className={`shrink-0 text-xs ${pageQuietText}`} {...pageChrome}>
                  {word}
                </span>
              )}
              {text === "" ? null : href === undefined ? (
                <span
                  className="min-w-0 [overflow-wrap:anywhere]"
                  {...(named ? authorText : pageChrome)}
                >
                  {drawn}
                </span>
              ) : (
                <Link
                  className={`dispatch-inline-link min-w-0 flex-1 [overflow-wrap:anywhere] ${ROW_LINK} ${pageInkText}`}
                  to={href}
                  {...(named ? authorText : pageChrome)}
                >
                  {drawn}
                </Link>
              )}
              {/* Where the citing node is of another issue or of a project, which one; and when
                  the reference was made. */}
              <span
                className={`shrink-0 text-xs whitespace-nowrap ${pageQuietText}`}
                data-reference-facts=""
              >
                {elsewhere ? (
                  <>
                    <span data-reference-source="" {...authorText}>
                      {row.source}
                    </span>
                    <span {...pageChrome}> · </span>
                  </>
                ) : null}
                <span data-reference-time="" {...authorText}>
                  {shortDay(row.at)}
                </span>
              </span>
            </li>
          );
        })}
      </ul>
      {pages.more(rows.length, undefined, "mt-1")}
    </>
  );
}
