import { useQuery } from "@tanstack/react-query";
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
import { type BlockItemKind, blockFragment, isBlockItemKind } from "./block-reference";
import { referenceTriggerProps } from "./RefPreview";
import { referencesQuery } from "./reference-target";
import type { DispatchReferenceRoute } from "./routes";
import { buildReferencePath, parseNodeReference } from "./routes";
import { Timestamp } from "./Timestamp";

/** Every node kind the server's read registry holds, which `GraphNode.kind` names, each with the
 *  word the panel reads it under. A kind a newer build adds is skipped where it is met, since no
 *  read answers a node of a kind its own build does not resolve. */
const NODE_LABELS: Record<GraphEdge["node"]["kind"], string> = {
  artifact: "Artifact",
  ask: "Ask",
  // A part of an ask, and a block of a comment or a message, read as the item that holds it, as
  // the `block` below does (`blockItem`); the server answers each as a node of kind `block`.
  ask_block: "Ask",
  // A type's attribute and a type itself: nodes of the block schema, which no read of the graph
  // answers today.
  attribute: "Attribute",
  // A block of a document that cites a node shows as the document holding it, linked to the
  // block; a part of an ask, or a block of a comment or message, shows as its item
  // (`blockItem`).
  block: "Artifact",
  comment: "Comment",
  comment_block: "Comment",
  component: "Component",
  // A constraint, a run of the command bar, a kind of the lens catalogue, a lens, a finding of a
  // lens and a risk read under the kind's own name until dispatch://INFRA-1196 gives them the
  // words a person reads.
  constraint: "constraint",
  interaction: "interaction",
  issue: "Issue",
  lens: "lens",
  lens_block: "lens_block",
  lens_kind: "lens_kind",
  message: "Message",
  message_block: "Message",
  // The page of a role, or of a session that claimed none: it reads as whose page it is,
  // because a reader opens the agent and not the page.
  page: "Agent page",
  person: "Person",
  // A recorded result of a check, which no read of the graph answers yet.
  result: "Result",
  risk: "risk",
  // A role is the agent Dispatch keeps across a restart, and a session is one run of one.
  role: "Agent",
  session: "Session",
  supertag: "Type",
};

const STRUCTURAL_LABELS: Partial<Record<GraphEdge["kind"], string>> = {
  anchored_to: "Anchored item",
  answers: "Answer",
  attached_to: "Attached document",
  // An issue's wait as a Dispatch of the release before names it; this Dispatch answers it as
  // needs, below.
  blocked_by: "Blocked issue",
  child_of: "Child issue",
  duplicate_of: "Duplicate issue",
  filed_from: "Issue filed from it",
  followed_by: "Follower",
  // A wait, one relation however its ends are (dispatch://INFRA-1253 R10): an issue's on an issue,
  // and the `needs` a plan task's `block[]` attribute makes on another (`blocks.json`), as a
  // check's or a plan task's `serves` names a requirement. Read from the node waited on, the edge
  // says what the citing node does for it.
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
  authored_by: true,
  blocked_by: true,
  changed_by_run: true,
  child_of: true,
  constrained_by: true,
  depends_on: true,
  duplicate_of: true,
  filed_from: true,
  followed_by: true,
  has_kind: true,
  mentions: true,
  needs: true,
  owned_by: true,
  part_of: true,
  prevented_by: true,
  replies_to: true,
  triggered_by: true,
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

/** Beyond this the row clamps the excerpt and offers to expand it. The real clamp is CSS, so
 *  this is the text shape that makes a clamp likely rather than a measurement. */
const CLAMP_LINES = 3;
const CLAMP_CHARACTERS = 240;

/** The ask, comment or message a graph node is a block of: its parent, for a block node naming
 *  no document (`ask/<uuid>#b2`, `comment/<uuid>#b2`, `message/<uuid>#b1`); null for any
 *  other node, as the read itself carries null for a node with no parent. A part of an ask reads
 *  as its ask, as a block of a comment reads as its comment: one set of item kinds says which
 *  (`isBlockItemKind`). */
function blockItem(
  node: GraphEdge["node"]
): (NonNullable<GraphEdge["node"]["parent"]> & { kind: BlockItemKind }) | null {
  const parent = node.parent;
  return node.kind === "block" &&
    node.document === undefined &&
    parent !== null &&
    isBlockItemKind(parent.kind)
    ? { ...parent, kind: parent.kind }
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
}: {
  text: string;
  tokens: { readonly excerpt: string; readonly link: string };
}): ReactNode {
  const [expanded, setExpanded] = useState(false);
  const clampable = text.split("\n").length > CLAMP_LINES || text.length > CLAMP_CHARACTERS;

  return (
    <>
      <p
        className={`mt-1 whitespace-pre-wrap text-sm ${expanded || !clampable ? "" : "line-clamp-3"} ${tokens.excerpt}`}
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

/** A row a host adds after the list's own, with its words already made: the list draws it as it
 *  draws its own rows and says nothing of what it is. */
export interface ExtraReferenceRow {
  /** The time the row reads, as `Timestamp` draws it. */
  readonly at: string;
  /** The relation the row's item stands in to the node, in the host's words. */
  readonly edge: string;
  /** The item's own words, drawn as the list draws an edge's excerpt. */
  readonly excerpt?: string;
  /** Where the row's link goes. */
  readonly href: string;
  readonly key: string;
  /** What kind of item the row is, in the host's words: the row's link. */
  readonly kind: string;
  /** Marks the host draws beside the kind. */
  readonly marks?: ReactNode;
}

/** The panel: one list of what references a node, read the first time a host renders it. Its
 *  host owns disclosure — `ReferencedByToggle` beside it, or nothing at all where the page shows
 *  the list outright — so this component has one behaviour. */
export function ReferencedBy({
  className,
  extraRows = [],
  page = false,
  reference,
  toggle,
}: {
  className?: string;
  /** Rows the host adds after the list's own. The list counts none of them as its own and draws
   *  them only while it holds one; while one stands, it does not say that nothing references the
   *  node. */
  extraRows?: readonly ExtraReferenceRow[];
  /** Whether the list is drawn in the new page's Details: in the page's colours, and without the
   *  issue's own asks, comments and messages, which that page's outline holds (`listedEdges`). */
  page?: boolean;
  reference: string;
  /** The control that discloses this panel: its `id` (which the control names in
   *  `aria-controls`) and the fact that it already reads `Referenced by (N)`, so the panel
   *  carries no heading of its own under it. Absent where the page shows the list outright,
   *  and then the panel is its own heading. */
  toggle?: { id: string };
}): ReactNode {
  const query = useQuery(referencesQuery(reference));
  const edges = listedEdges(query.data?.edges, reference, page);
  const groups = groupBySource(edges ?? []);
  const tokens = LIST_TOKENS[page ? "page" : "app"];

  return (
    <section aria-label="Referenced by" className={`space-y-2 ${className ?? ""}`} id={toggle?.id}>
      {toggle === undefined ? (
        <h3 className={`text-sm font-semibold ${tokens.heading}`}>
          Referenced by{edges === undefined ? "" : ` (${edges.length})`}
        </h3>
      ) : null}
      {query.isPending ? <p className={`text-sm ${tokens.line}`}>Loading references…</p> : null}
      {query.isError ? (
        <p className={`flex flex-wrap items-center gap-2 text-sm ${tokens.line}`}>
          <span>Referenced by unavailable.</span>
          <button
            className={`font-medium underline ${tokens.link}`}
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
            type="button"
          >
            Retry
          </button>
        </p>
      ) : null}
      {extraRows.length === 0 ? (
        // biome-ignore lint/complexity/noUselessFragments: it holds the empty list's line as its own conditional, under the extra rows' guard
        <>
          {edges?.length === 0 ? (
            <p className={`text-sm ${tokens.line}`}>Nothing references this yet.</p>
          ) : null}
        </>
      ) : null}
      {groups.length === 0 ? null : (
        <ul className="space-y-2">
          {groups.map((group) => (
            <li className={`rounded-lg border p-3 ${tokens.row}`} key={group.key}>
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
                  return (
                    <li
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
                      {edge.excerpt === undefined ? null : (
                        <Excerpt text={edge.excerpt.text} tokens={tokens} />
                      )}
                    </li>
                  );
                })}
              </ul>
            </li>
          ))}
        </ul>
      )}
      {extraRows.length === 0 ? null : (
        <ul className="space-y-2" data-extra-references="">
          {extraRows.map((row) => (
            <li className={`rounded-lg border p-3 ${tokens.row}`} key={row.key}>
              <div className={`text-xs font-medium ${tokens.line}`}>
                <Link className={`dispatch-inline-link underline ${tokens.link}`} to={row.href}>
                  {row.kind}
                </Link>
                {row.marks}
              </div>
              <div className={`flex flex-wrap items-center gap-x-1 text-xs ${tokens.line}`}>
                <span>{row.edge}</span>
                <span aria-hidden="true">·</span>
                <Timestamp at={row.at} />
              </div>
              {row.excerpt === undefined ? null : <Excerpt text={row.excerpt} tokens={tokens} />}
            </li>
          ))}
        </ul>
      )}
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
