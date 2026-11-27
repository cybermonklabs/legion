import { useQuery } from "@tanstack/react-query";
import type { Ref, RefObject } from "react";
import { type ReactNode, useId, useState } from "react";
import { Link } from "react-router-dom";

import type { DisplayTree, GraphEdge, GraphReferences } from "../../api/types";
import { heldPress } from "../../components/held-press";
import {
  borderDefault,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { compiledBlockSchema } from "../doc/schema";
import { blockFragment, isBlockItemKind } from "./block-reference";
import { MarkdownBody } from "./MarkdownBody";
import { MarkdownPreview } from "./MarkdownPreview";
import { DeletedBody, EditedMark } from "./PostMarks";
import { postDeletion } from "./post-marks";
import { referenceTriggerProps } from "./RefPreview";
import { referencesQuery, standsFailed, useNewIssuePageFreshness } from "./reference-target";
import type { DispatchReferenceRoute } from "./routes";
import { parseNodeReference, referenceHref } from "./routes";
import { Timestamp } from "./Timestamp";
import { type TextSource, textSourceKind, textSourceOf } from "./text-references";

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
 *  fragment when the reference names no block of its own. Undefined where it opens nothing: a
 *  reference that does not parse, and a party the graph records no node for (R5). */
function referencePath(reference: string | undefined, block?: string): SourcePath | undefined {
  const route = reference === undefined ? undefined : parseNodeReference(reference);
  const href = route === undefined ? undefined : referenceHref(route);
  if (route === undefined || href === undefined) {
    return undefined;
  }
  return {
    href: `${href}${
      ("block" in route && route.block !== undefined) || block === undefined
        ? ""
        : blockFragment(block)
    }`,
    route,
  };
}

/** An edge as the references read answers it, with its excerpt's tree (`GraphEdgeWithTrees`). */
type ReadEdge = NonNullable<GraphReferences["edges"]>[number];

interface SourceGroup {
  readonly edges: ReadEdge[];
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
function groupBySource(edges: readonly ReadEdge[]): SourceGroup[] {
  const groups = new Map<string, { edges: ReadEdge[]; nodes: Set<string> }>();
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
    const [first] = grouped as [ReadEdge, ...ReadEdge[]];
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

/** The citing nodes whose excerpt is the head of their own stored text. */
const BODY_NODE_KINDS: readonly string[] = ["ask", "block", "comment", "message"];

/** Whether `edge`'s excerpt is the head of a stored body rather than a title or a name. A citing
 *  block, ask, comment or message is listed by the head of its own text, and a document's
 *  mention by the block that holds it (`excerpt.block_id`): each is a bounded head the server
 *  cuts, which the renderer draws. A citing issue, document, component or session is listed by
 *  its title or name, which is one line of text already. */
function excerptIsBody(edge: GraphEdge): boolean {
  return edge.excerpt?.block_id !== undefined || BODY_NODE_KINDS.includes(edge.node.kind);
}

/** The record a body excerpt is the head of, where the citing node is one a `TextSource` names:
 *  an ask, a comment or a message, or a document. A citing block is a block of its holder - a
 *  comment, an ask, a message or a document - which the node carries as its `parent`
 *  (`refs/graph.go`), so its excerpt is the holder's text and a `#id` in it names a question of
 *  the holder's document. */
function excerptSource(node: GraphEdge["node"]): TextSource | undefined {
  const holder = node.kind === "block" ? node.parent : node;
  return holder === null
    ? undefined
    : textSourceOf(textSourceKind(holder.kind), holder.id, node.issue_key);
}

function Excerpt({
  rendered,
  source,
  text,
  tree,
}: {
  /** Whether the text is the head of a stored body, which the renderer draws (`excerptIsBody`). */
  rendered: boolean;
  source: TextSource | undefined;
  text: string;
  /** The server's reading of the head as it cut it, so the page parses none of it. */
  tree: DisplayTree | undefined;
}): ReactNode {
  const [expanded, setExpanded] = useState(false);
  const clampable = text.split("\n").length > CLAMP_LINES || text.length > CLAMP_CHARACTERS;
  const clamped = clampable && !expanded;

  return (
    <>
      {rendered ? (
        // The head of a stored body, drawn by the renderer from the reading the server made of
        // it: one clamped run until the reader opens it, the whole of it after.
        // The type is on the element around the renderer in both, as `.dispatch-markdown` takes
        // it from there (styles.css).
        clamped ? (
          <div className={`mt-1 text-sm ${textSecondaryOnSurface}`}>
            <MarkdownPreview
              lines={3}
              markdown={text}
              source={source}
              surface="referenced-by-excerpt"
              tree={tree}
            />
          </div>
        ) : (
          <div className={`mt-1 text-sm ${textSecondaryOnSurface}`}>
            <MarkdownBody
              markdown={text}
              source={source}
              surface="referenced-by-excerpt"
              tree={tree}
            />
          </div>
        )
      ) : (
        <p
          className={`mt-1 whitespace-pre-wrap text-sm ${clamped ? "line-clamp-3" : ""} ${textSecondaryOnSurface}`}
        >
          {/* A title or a name, as the line it is: the reading list's `referenced-by-title`. */}
          {text}
        </p>
      )}
      {clampable ? (
        <button
          aria-expanded={expanded}
          className={`min-h-11 text-xs font-medium ${linkText} ${linkHoverText}`}
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
 *  `marks` are the host's own handles on the button, by which its keys find and press it: the
 *  Inbox's ask card passes the mark its command bar presses it by (`inboxActionMark`), inside the
 *  item in hand.
 *
 *  A block of a document has no such count - no listing the tabs read carries one - so `count`
 *  is left out there and the control offers itself without one, the panel's own read being the
 *  first thing that knows. */
export function ReferencedByToggle({
  controls,
  count,
  expanded,
  marks,
  onToggle,
}: {
  controls: string;
  count?: number;
  expanded: boolean;
  marks?: Readonly<Record<`data-${string}`, string>>;
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
      {...marks}
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
  reference,
  retried,
  retryRef,
  toggle,
}: {
  className?: string;
  reference: string;
  /** Notes the reader's press of Retry for the hand-off of its host (`useFocusHandOff`'s
   *  `pressed`): set as Retry is pressed, and forgotten where the list still reads as failed once
   *  the read the press sent, or joined, has ended, since that press then took nothing away. Where
   *  it is passed, the failure line stands while the list holds no answer and a read of it has
   *  failed (`standsFailed`), in place of the loading line, as Details' own lines do, so the line
   *  stays while the read Retry sent is out, and Retry keeps its focus then, and a press while
   *  the read is out joins it (`cancelRefetch: false`) and sends nothing new. The new issue
   *  page's Details passes it. Elsewhere the line stands while the last read failed, and a press
   *  while the read is out does nothing. On every page Retry is held while its read is out, as
   *  `heldPress` holds a control: `aria-disabled`, never `disabled`. */
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
  // During a release a newer Dispatch can answer with an edge kind, or a node kind at the other
  // end, that this page does not know; the page leaves such an edge out rather than draw it under
  // another edge's label.
  const edges = query.data?.edges?.filter(
    (edge) =>
      (Object.hasOwn(RENDERED_EDGE_KINDS, edge.kind) || MANIFEST_EDGE_KINDS.has(edge.kind)) &&
      Object.hasOwn(NODE_LABELS, edge.node.kind)
  );
  const groups = groupBySource(edges ?? []);
  // Retry's attributes, one object for each host, spread whole. Every page holds Retry while its
  // read is out (`aria-disabled`), as `heldPress` does, and outside Details a held Retry's press
  // does nothing. Details' held Retry still takes a press, which joins the read that is out
  // (`cancelRefetch: false`), so it sends nothing new, and notes the press for the host's hand-off
  // (`retried`): Details keeps the failure line, and Retry its focus, while Retry's read is out
  // (below), so the reader can press Retry again then, and the hand-off reads that press once the
  // read has ended.
  const retryPress =
    retried === undefined
      ? heldPress(query.isFetching, () => void query.refetch())
      : {
          "aria-disabled": query.isFetching || undefined,
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
        <h3 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>
          Referenced by{edges === undefined ? "" : ` (${edges.length})`}
        </h3>
      ) : null}
      {query.isPending && !failed ? (
        <p className={`text-sm ${textMutedOnSurface}`}>Loading references…</p>
      ) : null}
      {failed ? (
        <p className={`flex flex-wrap items-center gap-2 text-sm ${textMutedOnSurface}`}>
          <span>Referenced by unavailable.</span>
          {/* Held while a read of the list is out (`heldPress`), never disabled: it keeps the
              focus and its Tab stop, and a press of it sends nothing then. */}
          <button
            className={`font-medium underline refused:cursor-not-allowed refused:opacity-50 ${linkText} ${linkHoverText}`}
            {...retryPress}
            ref={retryRef}
            type="button"
          >
            Retry
          </button>
        </p>
      ) : null}
      {edges?.length === 0 ? (
        <p className={`text-sm ${textMutedOnSurface}`}>Nothing references this yet.</p>
      ) : null}
      {groups.length === 0 ? null : (
        <ul className="space-y-2">
          {groups.map((group) => (
            <li className={`rounded-lg border p-3 ${borderDefault}`} key={group.key}>
              <div className={`text-xs font-medium ${textMutedOnSurface}`}>
                {group.path === undefined ? (
                  <span>{group.label}</span>
                ) : (
                  <Link
                    {...referenceTriggerProps(group.path.route)}
                    className={`dispatch-inline-link underline ${linkText} ${linkHoverText}`}
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
                      key={`${edge.node.kind}:${edge.node.id}:${edge.kind}:${edge.direction}:${edge.block?.id ?? ""}`}
                    >
                      <div
                        className={`flex flex-wrap items-center gap-x-1 text-xs ${textMutedOnSurface}`}
                      >
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
                            className={`underline ${linkText} ${linkHoverText}`}
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
                              className={`underline ${linkText} ${linkHoverText}`}
                              to={cited.href}
                            >
                              block {edge.block?.block_id}
                            </Link>
                          </>
                        )}
                      </div>
                      {/* A block whose text opens with a reference longer than the server's cut
                          has an empty excerpt that still names the block it links to: the row
                          then draws no excerpt rather than an empty paragraph. */}
                      {deleted !== null ? (
                        <DeletedBody className="mt-1 block text-sm" deleted={deleted} />
                      ) : edge.excerpt === undefined || edge.excerpt.text === "" ? null : (
                        <Excerpt
                          rendered={excerptIsBody(edge)}
                          source={excerptSource(edge.node)}
                          text={edge.excerpt.text}
                          tree={edge.excerpt.text_tree}
                        />
                      )}
                    </li>
                  );
                })}
              </ul>
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
