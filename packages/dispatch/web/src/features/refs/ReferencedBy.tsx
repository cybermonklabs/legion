import { useQuery } from "@tanstack/react-query";
import type { ButtonHTMLAttributes, Ref, RefObject } from "react";
import { type ReactNode, useId, useState } from "react";
import { Link } from "react-router-dom";

import type { GraphEdge } from "../../api/types";
import {
  borderDefault,
  linkHoverText,
  linkText,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pageQuietText,
  pageRowAct,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { compiledBlockSchema } from "../doc/schema";
import { blockFragment, isBlockItemKind } from "./block-reference";
import { DeletedBody, EditedMark } from "./PostBody";
import { postDeletion } from "./post-marks";
import { referenceTriggerProps } from "./RefPreview";
import { referencesQuery, standsFailed, useNewIssuePageFreshness } from "./reference-target";
import type { DispatchReferenceRoute } from "./routes";
import { buildReferencePath, parseNodeReference } from "./routes";
import { Timestamp } from "./Timestamp";

const NODE_LABELS: Record<GraphEdge["node"]["kind"], string> = {
  artifact: "Artifact",
  ask: "Ask",
  // A block of a document that cites a node shows as the document holding it, linked to the
  // block; a part of an ask, or a block of a comment or message, shows as its item
  // (`blockItem`).
  block: "Artifact",
  comment: "Comment",
  component: "Component",
  issue: "Issue",
  message: "Message",
  // The page of a role, or of a session that claimed none: it reads as whose page it is,
  // because a reader opens the agent and not the page.
  page: "Agent page",
  person: "Person",
  // A role is the agent Dispatch keeps across a restart, and a session is one run of one.
  role: "Agent",
  session: "Session",
};

const STRUCTURAL_LABELS: Partial<Record<GraphEdge["kind"], string>> = {
  anchored_to: "Anchored item",
  answers: "Answer",
  attached_to: "Attached document",
  blocked_by: "Blocked issue",
  child_of: "Child issue",
  followed_by: "Follower",
  // The two kinds a typed block's `block[]` attributes make (`blocks.json`): a check or a plan
  // task that names a requirement in `serves`, and a plan task that names another in `needs`.
  // Read from the named block, the edge says what the citing block does for it.
  needs: "needed by",
  owned_by: "Owned item",
  replies_to: "Reply",
  serves: "served by",
};

/** Every structural edge kind the page renders: all the ones `GraphEdgeKind` names. */
const RENDERED_EDGE_KINDS: Record<GraphEdge["kind"], true> = {
  affects: true,
  anchored_to: true,
  answers: true,
  attached_to: true,
  blocked_by: true,
  child_of: true,
  depends_on: true,
  followed_by: true,
  mentions: true,
  owned_by: true,
  part_of: true,
  replies_to: true,
};

/** The other kinds the page renders: the edge kinds the `block[]` attributes of the manifest
 *  this page was built with make, each such attribute's name, as Go's `pmdoc.LinkKinds` reads
 *  them. A kind only a newer build's manifest declares is not among them, so the page leaves it
 *  out rather than draw it under another edge's label. */
const MANIFEST_EDGE_KINDS = new Set(
  compiledBlockSchema.types.flatMap((type) =>
    Object.entries(type.attributes)
      .filter(([, attribute]) => attribute.kind === "block[]")
      .map(([name]) => name)
  )
);

/** Whether a list of what references a node draws `edge`. During a release a newer Dispatch can
 *  answer with an edge kind, or a node kind at the other end, that this page does not know; every
 *  such list - this panel, and a focused row's in Details (`NodeReferences.tsx`) - leaves that
 *  edge out rather than draw it under another edge's label. */
export function drawsEdge(edge: GraphEdge): boolean {
  return (
    (Object.hasOwn(RENDERED_EDGE_KINDS, edge.kind) || MANIFEST_EDGE_KINDS.has(edge.kind)) &&
    Object.hasOwn(NODE_LABELS, edge.node.kind)
  );
}

/** The edges a list of what references `reference` shows of a read's `edges`: those it draws
 *  (`drawsEdge`), and with `page` none whose citing item - the node, or the ask, comment or
 *  message a block of one belongs to (`blockItem`) - is an ask, comment or message of the issue
 *  `reference` names, since the new page's outline is that issue's conversation. One function, so
 *  a control that counts the list counts the rows the list shows. Undefined while there is no
 *  read. */
export function listedEdges(
  edges: readonly GraphEdge[] | null | undefined,
  reference: string,
  page: boolean
): GraphEdge[] | undefined {
  const drawn = edges?.filter(drawsEdge);
  const route = page ? parseNodeReference(reference) : undefined;
  const issueKey = route !== undefined && "key" in route ? route.key : undefined;
  if (issueKey === undefined) {
    return drawn;
  }
  return drawn?.filter(
    (edge) =>
      !isBlockItemKind((blockItem(edge.node) ?? edge.node).kind) || edge.node.issue_key !== issueKey
  );
}

/** The colours of the list's heading, its lines, its rows, its excerpts and its links: the
 *  application's, and with `page` the new page's own tokens (`PAGE_COLORS`), as every colour of
 *  Details is - a link in the page's ink, underlined under the pointer as the page's row controls
 *  are, and an excerpt in its muted grey. Its words are the application's on both. */
const LIST_TOKENS = {
  app: {
    excerpt: textSecondaryOnSurface,
    heading: textSecondaryOnSurface,
    line: textMutedOnSurface,
    link: `${linkText} ${linkHoverText}`,
    row: borderDefault,
  },
  page: {
    excerpt: pageMutedText,
    heading: pageMutedText,
    line: pageQuietText,
    link: `${pageInkText} ${pageRowAct}`,
    row: pageLineBorder,
  },
} as const;

/** The look of an excerpt's words (`Excerpt`), clamped or drawn whole: one list for both. */
const EXCERPT_WORDS = "mt-1 whitespace-pre-wrap text-sm";

/** Beyond this the row clamps the excerpt and offers to expand it. The real clamp is CSS, so
 *  this is the text shape that makes a clamp likely rather than a measurement. */
const CLAMP_LINES = 3;
const CLAMP_CHARACTERS = 240;

/** The ask, comment or message a graph node is a block of: its parent, for a block node naming
 *  no document (`ask/<uuid>#b2`, `comment/<uuid>#b2`, `message/<uuid>#b1`); null for any
 *  other node, as the read itself carries null for a node with no parent. A part of an ask reads
 *  as its ask, as a block of a comment reads as its comment: one set of item kinds says which
 *  (`isBlockItemKind`). */
function blockItem(node: GraphEdge["node"]): GraphEdge["node"]["parent"] {
  return node.kind === "block" && node.document === undefined && isBlockItemKind(node.parent?.kind)
    ? node.parent
    : null;
}

function edgeLabel(edge: GraphEdge): string {
  return STRUCTURAL_LABELS[edge.kind] ?? NODE_LABELS[blockItem(edge.node)?.kind ?? edge.node.kind];
}

function sourceLabel(edge: GraphEdge): string {
  const source = edge.node.issue_key ?? edge.node.project ?? edge.node.id;
  return `${edgeLabel(edge)} · ${source}`;
}

interface SourcePath {
  readonly href: string;
  readonly route: DispatchReferenceRoute;
}

/** Where `reference`, a node's own reference as the graph returned it, opens - whatever the length
 *  of its name - with `block` (the block of a document that holds the mention) as its `#b-`
 *  fragment when the reference names no block of its own. */
function referencePath(reference: string | undefined, block?: string): SourcePath | undefined {
  const route = reference === undefined ? undefined : parseNodeReference(reference);
  if (route === undefined) {
    return undefined;
  }
  return {
    href: `${buildReferencePath(route)}${
      ("block" in route && route.block !== undefined) || block === undefined
        ? ""
        : blockFragment(block)
    }`,
    route,
  };
}

interface SourceGroup {
  readonly edges: GraphEdge[];
  readonly key: string;
  readonly label: string;
  /** How many citing nodes the group holds: more than one only when blocks of one document, or of
   *  one comment or message, cite the node, each then shown as a row linked to its own block. */
  readonly nodes: number;
  readonly path: SourcePath | undefined;
}

/** One entry per citing source, in the server's newest-first order: a document that both holds
 *  an ask's anchor and mentions it is one source with two rows, not two cards, and the blocks of
 *  one document, or the parts or blocks of one ask, comment or message, that cite the node are
 *  that document's or item's rows. A source of one node links to that node (a block at the
 *  block); a document or item whose blocks are its rows links to itself, and each row to its own
 *  block. */
function groupBySource(edges: readonly GraphEdge[]): SourceGroup[] {
  const groups = new Map<string, { edges: GraphEdge[]; nodes: Set<string> }>();
  for (const edge of edges) {
    const key =
      edge.node.document ??
      blockItem(edge.node)?.ref ??
      edge.node.ref ??
      `${edge.node.kind}:${edge.node.id}`;
    const group = groups.get(key) ?? { edges: [], nodes: new Set<string>() };
    group.edges.push(edge);
    group.nodes.add(`${edge.node.kind}:${edge.node.id}`);
    groups.set(key, group);
  }
  return [...groups].map(([key, { edges: grouped, nodes }]) => {
    const [first] = grouped as [GraphEdge, ...GraphEdge[]];
    return {
      edges: grouped,
      key,
      label: sourceLabel(first),
      nodes: nodes.size,
      path:
        nodes.size === 1
          ? referencePath(first.node.ref, first.excerpt?.block_id)
          : referencePath(key),
    };
  });
}

function Excerpt({
  text,
  tokens,
  whole = false,
}: {
  text: string;
  tokens: { readonly excerpt: string; readonly link: string };
  /** Whether the excerpt is drawn whole, as the new page's Details draws each one (`page`): with
   *  no clamp and no Show more, a long word broken where it must be rather than run past its row.
   *  Every other page passes nothing, and the excerpt clamps as it always has. */
  whole?: boolean;
}): ReactNode {
  const [expanded, setExpanded] = useState(false);
  const clampable = text.split("\n").length > CLAMP_LINES || text.length > CLAMP_CHARACTERS;
  if (whole) {
    return <p className={`${EXCERPT_WORDS} [overflow-wrap:anywhere] ${tokens.excerpt}`}>{text}</p>;
  }

  return (
    <>
      <p
        className={`${EXCERPT_WORDS} ${expanded || !clampable ? "" : "line-clamp-3"} ${tokens.excerpt}`}
      >
        {text}
      </p>
      {clampable ? (
        <button
          aria-expanded={expanded}
          className={`min-h-11 text-xs font-medium ${tokens.link}`}
          onClick={() => setExpanded((open) => !open)}
          type="button"
        >
          {expanded ? "Show less" : "Show more"}
        </button>
      ) : null}
    </>
  );
}

/** The control every host uses: the count comes from the list response the page already read,
 *  so a card or a header costs no graph request until the reader opens its panel. A count of
 *  zero is a fact, not an affordance: it renders as plain text, since opening it would only say
 *  nothing references the node. `controls` names the panel the host renders when expanded.
 *
 *  A block of a document has no such count - no listing the tabs read carries one - so `count`
 *  is left out there and the control offers itself without one, the panel's own read being the
 *  first thing that knows. */
export function ReferencedByToggle({
  controls,
  count,
  expanded,
  onToggle,
}: {
  controls: string;
  count?: number;
  expanded: boolean;
  onToggle: () => void;
}): ReactNode {
  if (count === 0) {
    return (
      <span
        className={`inline-flex min-h-11 shrink-0 items-center px-2 py-1 text-sm ${textMutedOnSurface}`}
      >
        Referenced by (0)
      </span>
    );
  }

  return (
    <button
      aria-controls={controls}
      aria-expanded={expanded}
      className={`flex min-h-11 shrink-0 items-center gap-2 rounded-lg px-2 py-1 text-sm font-medium ${linkText} ${linkHoverText}`}
      onClick={onToggle}
      type="button"
    >
      {count === undefined ? "Referenced by" : `Referenced by (${count})`}
    </button>
  );
}

/** The panel: one list of what references a node, read the first time a host renders it. Its
 *  host owns disclosure — `ReferencedByToggle` beside it, or nothing at all where the page shows
 *  the list outright — so this component has one behaviour. */
export function ReferencedBy({
  className,
  page = false,
  pages,
  reference,
  retried,
  retryRef,
  toggle,
}: {
  className?: string;
  /** Whether the list is drawn in the new page's Details: in the page's colours, and without the
   *  issue's own asks, comments and messages, which that page's outline holds (`listedEdges`). */
  page?: boolean;
  /** With `page`, the pages its rows are drawn in, which Details holds (`useListPages`): how
   *  many rows the list draws, and the control under them that draws the next ones, which says
   *  the page's words. The control is handed how many rows the whole list holds and the element
   *  of each row by its place, which its press moves the focus to. Without it every row is
   *  drawn. */
  pages?: {
    readonly drawn: number;
    readonly more: (
      count: number,
      entry: (index: number) => Element | null | undefined
    ) => ReactNode;
  };
  reference: string;
  /** Notes the reader's press of Retry for the hand-off of its host (`useFocusHandOff`'s
   *  `pressed`): set as Retry is pressed, and forgotten where the list still reads as failed once
   *  the read the press sent, or joined, has ended, since that press then took nothing away. Where
   *  it is passed, the failure line stands while the list holds no answer and a read of it has
   *  failed (`standsFailed`), in place of the loading line, as Details' own lines do, so the line
   *  stays while the read Retry sent is out, and Retry keeps its focus then: it is drawn
   *  `aria-disabled`, in place of `disabled`, since Chromium moves the focus of a control that
   *  turns disabled to the page's body, and a press while the read is out joins it
   *  (`cancelRefetch: false`) and sends nothing new. The new issue page's Details passes it.
   *  Elsewhere the line stands while the last read failed, and Retry is `disabled` while its read
   *  is out. */
  retried?: RefObject<boolean>;
  /** Takes Retry, so the host can hand the focus it holds on as it goes (`useFocusHandOff`). */
  retryRef?: Ref<HTMLButtonElement>;
  /** The control that discloses this panel: its `id` (which the control names in
   *  `aria-controls`) and the fact that it already reads `Referenced by (N)`, so the panel
   *  carries no heading of its own under it. Absent where the page shows the list outright,
   *  and then the panel is its own heading. */
  toggle?: { id: string };
}): ReactNode {
  const freshness = useNewIssuePageFreshness();
  const query = useQuery({ ...referencesQuery(reference), ...freshness });
  const edges = listedEdges(query.data?.edges, reference, page);
  const groups = groupBySource(edges ?? []);
  const tokens = LIST_TOKENS[page ? "page" : "app"];
  // With `page` and `pages`, the list's rows are one sequence, each source's rows in the order
  // its card draws them, of which the list draws the first `pages.drawn`. Each source draws
  // those of its rows that fit in what is left of that number (`rowsLeft`), in its place in the
  // sequence, and counts every one of its rows in the list's (`rowCount`), the count the
  // control under the list is handed. Another source of rows drawn after these takes and counts
  // its rows the same way, and shares the list's pages, its control and its count. A card the
  // cut falls in draws the rows before the cut, and a card after the cut is not drawn.
  const paged = page ? pages : undefined;
  let rowCount = 0;
  if (paged !== undefined) {
    let rowsLeft = paged.drawn;
    const drawnGroups = groups.flatMap((group) => {
      rowCount += group.edges.length;
      const drawnEdges = group.edges.slice(0, rowsLeft);
      rowsLeft -= drawnEdges.length;
      return drawnEdges.length === 0 ? [] : [{ ...group, edges: drawnEdges }];
    });
    groups.splice(0, groups.length, ...drawnGroups);
  }
  // Each row drawn in pages carries the list's own mark, and each card its card mark
  // (`data-reference-card`), by which the press of the control finds the first row it drew, by
  // its place in the sequence: the row's card where the row is its card's first, whose link
  // names the source, and else the row itself.
  const rowsMark = useId();
  const rowAt = (index: number) => {
    const marked = `[data-reference-rows="${CSS.escape(rowsMark)}"]`;
    const row = document.querySelectorAll(marked)[index];
    const card = row?.closest("[data-reference-card]");
    return card?.querySelector(marked) === row ? card : row;
  };
  // Retry's attributes where the host notes its press (`retried`), spread over the ones it has on
  // every page.
  const retryInDetails:
    | Pick<ButtonHTMLAttributes<HTMLButtonElement>, "aria-disabled" | "disabled" | "onClick">
    | undefined =
    retried === undefined
      ? undefined
      : {
          "aria-disabled": query.isFetching || undefined,
          disabled: false,
          onClick: () => {
            retried.current = true;
            void query.refetch({ cancelRefetch: false }).then((answer) => {
              if (answer.isError) retried.current = false;
            });
          },
        };
  // Where the host notes Retry's press - Details - the line stands while the read stands failed
  // (`standsFailed`), as Details says each of its own reads failed: a refresh that fails over the
  // list keeps the list and draws no line over it, and the line stays while Retry's read is out,
  // so Retry keeps its focus. While the line stands in Details it takes the place of the loading
  // line, as Details' own lines do while their Retry's read is out. Every other page draws the
  // line while the last read failed, which is never while the read is pending, so the loading line
  // there is as before.
  const failed = retried === undefined ? query.isError : standsFailed(query);

  return (
    <section aria-label="Referenced by" className={`space-y-2 ${className ?? ""}`} id={toggle?.id}>
      {toggle === undefined ? (
        <h3 className={`text-sm font-semibold ${tokens.heading}`}>
          Referenced by{edges === undefined ? "" : ` (${edges.length})`}
        </h3>
      ) : null}
      {query.isPending && !failed ? (
        <p className={`text-sm ${tokens.line}`}>Loading references…</p>
      ) : null}
      {failed ? (
        <p className={`flex flex-wrap items-center gap-2 text-sm ${tokens.line}`}>
          <span>Referenced by unavailable.</span>
          <button
            className={`font-medium underline ${tokens.link}`}
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
            {...retryInDetails}
            ref={retryRef}
            type="button"
          >
            Retry
          </button>
        </p>
      ) : null}
      {edges?.length === 0 ? (
        <p className={`text-sm ${tokens.line}`}>Nothing references this yet.</p>
      ) : null}
      {groups.length === 0 ? null : (
        <ul className="space-y-2">
          {groups.map((group) => (
            <li
              className={`rounded-lg border p-3 ${tokens.row}`}
              data-reference-card={paged === undefined ? undefined : ""}
              key={group.key}
            >
              <div className={`text-xs font-medium ${tokens.line}`}>
                {group.path === undefined ? (
                  <span>{group.label}</span>
                ) : (
                  <Link
                    {...referenceTriggerProps(group.path.route)}
                    className={`dispatch-inline-link underline ${tokens.link}`}
                    to={group.path.href}
                  >
                    {group.label}
                  </Link>
                )}
              </div>
              <ul className="space-y-2">
                {group.edges.map((edge) => {
                  const row =
                    group.nodes === 1
                      ? undefined
                      : referencePath(edge.node.ref, edge.excerpt?.block_id);
                  // On a document's read, the block of the document the edge meets: one source
                  // citing two of its blocks is two rows, each naming and keyed by its block.
                  const cited = referencePath(edge.block?.ref);
                  // A citing comment or message, or a block of one, carries its post's marks: a
                  // deleted one has no excerpt left, and the deleted line takes its place.
                  const deleted = postDeletion(edge.node);
                  return (
                    <li
                      data-reference-rows={paged === undefined ? undefined : rowsMark}
                      key={`${edge.node.kind}:${edge.node.id}:${edge.kind}:${edge.direction}:${edge.block?.id ?? ""}`}
                    >
                      <div className={`flex flex-wrap items-center gap-x-1 text-xs ${tokens.line}`}>
                        {group.edges.length === 1 ? null : (
                          <>
                            <span>{edgeLabel(edge)}</span>
                            <span aria-hidden="true">·</span>
                          </>
                        )}
                        {row === undefined ? (
                          <Timestamp at={edge.created_at} />
                        ) : (
                          <Link
                            {...referenceTriggerProps(row.route)}
                            className={`underline ${tokens.link}`}
                            to={row.href}
                          >
                            <Timestamp at={edge.created_at} />
                          </Link>
                        )}
                        <EditedMark at={edge.node.edited_at} deleted={deleted} />
                        {cited === undefined ? null : (
                          <>
                            <span aria-hidden="true">·</span>
                            <Link
                              {...referenceTriggerProps(cited.route)}
                              className={`underline ${tokens.link}`}
                              to={cited.href}
                            >
                              block {edge.block?.block_id}
                            </Link>
                          </>
                        )}
                      </div>
                      {deleted !== null ? (
                        <DeletedBody className="mt-1 block text-sm" deleted={deleted} />
                      ) : edge.excerpt === undefined ? null : (
                        <Excerpt text={edge.excerpt.text} tokens={tokens} whole={page} />
                      )}
                    </li>
                  );
                })}
              </ul>
            </li>
          ))}
        </ul>
      )}
      {paged === undefined ? null : paged.more(rowCount, rowAt)}
    </section>
  );
}

/** The Referenced-by disclosure a typed block carries on the Spec, Verification and Plan tabs:
 *  the control, and under it, once the reader opens it, the panel that reads what points at that
 *  block. No listing those tabs read carries the block's reference count, so the control shows
 *  none and nothing is read until it is opened. */
export function BlockReferences({ reference }: { reference: string }): ReactNode {
  const panelId = useId();
  const [open, setOpen] = useState(false);

  return (
    <div className="space-y-2">
      <ReferencedByToggle
        controls={panelId}
        expanded={open}
        onToggle={() => setOpen((shown) => !shown)}
      />
      {open ? <ReferencedBy reference={reference} toggle={{ id: panelId }} /> : null}
    </div>
  );
}
