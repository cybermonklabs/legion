import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec, roleDocument } from "../../../api/issue-cache";
import { referenceTargetBatches, referenceTargetsQuery, TREE_FILTERS } from "../../../api/queries";
import type { Artifact, IssueDetails, ReferenceTarget } from "../../../api/types";
import { buildDispatchReference, type IssueRoute } from "../../refs/routes";
import type { IssueAddress } from "../issue-address";
import type { TreeNode } from "../issue-tree";
import { answerHolds } from "../presets";
import { lastHeldOf } from "../tree-drafts";
import { nextTreeCursor } from "../tree-read/tree-nodes";
import { type TreeAnswer, viewQuery } from "../tree-read/tree-read";
import { lastHeldPath } from "./last-held";
import { lastHeldAsk, lastHeldOpening } from "./last-held-ask";
import { useKnownLastHeldReferences } from "./last-held-store";

/**
 * Where an address of the issue page opens instead whose `#b-` fragment names a block the reads
 * its view draws from lack (R9 and R12 of Graph editing), by what `reference-targets` answers of
 * the block: a document's own address (`useLastHeldAddress`), and the issue's own address, Graph's
 * and the Plan and Verification filters' (`useUnqualifiedLastHeld`). Each opens the version that
 * last held the block, or the address without it, with no line. `slots/body.tsx` calls both.
 */

/**
 * Where a document's own address opens instead (`/issues/<KEY>/artifacts/<slug>#b-<id>`, at no
 * version) whose block the outline read lacks (`lastHeldAsk`), the outline being what the page
 * draws at that address (`slots/body.tsx`): the version that last held the block, or the address
 * without it, with no line; undefined while the address stands as it is.
 *
 * The page's names read (`useIssueTree`) adds a block to its set for the Spec view's own address
 * alone. Where the tree cites this one (`cites`), that read names it already, and its answer, as
 * the page publishes it (`useKnownLastHeldReferences`), opens the address: the block gone, with
 * the version that last held it or with none (`lastHeldPath`); named live, or not answered yet, the
 * address stands, and no second read of it is sent. Where the tree cites it nowhere, this is the
 * one read of it: one `reference-targets` read of the block's reference alone
 * (`referenceTargetsQuery`), sent once and never again for that address, which its answer
 * replaces (`lastHeldOpening`). The address it opens pins a version and asks nothing. While the
 * read is out, or where it is refused, the address stands.
 */
export function useLastHeldAddress({
  address,
  cites,
  held,
  reveal,
  route,
}: {
  readonly address: Pick<IssueAddress, "target" | "version">;
  /** Whether the tree cites `reference`, which the page's names read then names. */
  readonly cites: (reference: string) => boolean;
  /** The outline read, every page of it read so far; undefined while it holds no answer, or
   *  while a read the tree is built from is out. */
  readonly held: TreeAnswer | undefined;
  /** The chain the tree holds down to the address's node (`useAddressTarget`). */
  readonly reveal: readonly TreeNode[] | undefined;
  readonly route: IssueRoute;
}): string | undefined {
  const { hash, pathname, search } = useLocation();
  const history = useKnownLastHeldReferences();
  const asked =
    route.kind === "artifact" ? lastHeldAsk({ address, hash, held, reveal, route }) : undefined;
  const cited = useMemo(() => asked !== undefined && cites(asked), [asked, cites]);
  const answer = useQuery({
    ...referenceTargetsQuery(asked === undefined || cited ? [] : [asked]),
    enabled: asked !== undefined && !cited,
  }).data;
  const unblocked = `${pathname}${search}`;
  if (asked === undefined) return undefined;
  if (cited) {
    return history.has(asked) ? (lastHeldPath(history.get(asked)) ?? unblocked) : undefined;
  }
  if (answer === undefined) return undefined;
  return lastHeldOpening(
    answer.targets.find((one) => one.ref === asked),
    unblocked
  );
}

/** The routes whose `#b-` fragment names its block in no document (the architect's decision on
 *  plan task t12 of Graph editing): the issue's own address, Graph's, zoomed to a node or not,
 *  and the Plan and Verification filters'. An address of any other route, the Decisions filter's
 *  among them, keeps the outline's line where no row stands for its block (`slots/body.tsx`). */
const UNQUALIFIED_ROUTES: Readonly<Partial<Record<IssueRoute["kind"], true>>> = {
  graph: true,
  issue: true,
  plan: true,
  verification: true,
};

/**
 * The first of `references`, in their order, that one `reference-targets` request carries: the
 * first batch `referenceTargetsQuery` would read them in (`referenceTargetBatches`), at most
 * `REFERENCE_TARGETS_MAX` references, the server's cap, and at most
 * `REFERENCE_TARGETS_MAX_QUERY_CHARS` characters of query string, but for a first address longer
 * than that budget, which goes alone. What a set writes does not hang on its order, so the set
 * that query sorts is one request too.
 */
export function withinOneRead(references: readonly string[]): readonly string[] {
  return referenceTargetBatches(references)[0] ?? [];
}

/**
 * What the page asks `reference-targets` for a block an address names by a `#b-` fragment of no
 * document (`unqualifiedAsk`): the block in each document of the issue it can read, each once and
 * in the form the server reads (`buildDispatchReference`), the spec's as `spec`, any other's
 * under `artifact/<slug>`. The spec, the Verification document and the plan (`roleDocument`)
 * come first, then the issue's tree artifact, the one the tree read names (`tree`), then each
 * other document (an artifact of kind `doc`) in the issue's order, so the ones `withinOneRead`
 * cuts past the bounds of one read are the last.
 */
export function blockReferences({
  block,
  issue,
  key,
  tree,
}: {
  /** The block's id, as the fragment names it. */
  readonly block: string;
  readonly issue: Pick<
    IssueDetails,
    "artifacts" | "plan_artifact_id" | "primary_artifact_id" | "verification_artifact_id"
  >;
  readonly key: string;
  /** The id of the issue's tree artifact, as the tree read names it: null where the issue has
   *  not converted. */
  readonly tree: string | null | undefined;
}): readonly string[] {
  const target = { id: block };
  const artifacts = issue.artifacts ?? [];
  const spec = primarySpec(issue);
  const inDocument = (document: Pick<Artifact, "slug"> | undefined): string | undefined =>
    document === undefined
      ? undefined
      : buildDispatchReference({ block: target, key, kind: "artifact", slug: document.slug });
  const treeArtifact =
    tree === null || tree === undefined
      ? undefined
      : artifacts.find((artifact) => artifact.id === tree);
  const ordered = [
    spec === undefined ? undefined : buildDispatchReference({ block: target, key, kind: "spec" }),
    inDocument(roleDocument(issue, "verification")),
    inDocument(roleDocument(issue, "plan")),
    inDocument(treeArtifact),
    ...artifacts
      .filter((artifact) => artifact.kind === "doc" && artifact.id !== spec?.id)
      .map(inDocument),
  ];
  return withinOneRead([...new Set(ordered.filter((ref): ref is string => ref !== undefined))]);
}

/**
 * The references the page asks `reference-targets` for once where an address names, by a `#b-`
 * fragment of no document, a block no read of the page holds (R9 and R12 of Graph editing, the
 * architect's decision on plan task t12): its route is one of `UNQUALIFIED_ROUTES`, it pins no
 * version, each read the address's view draws from (`held`) holds an answer that names no page
 * more (`nextTreeCursor`), and neither those nor any other read the page holds (`others`) holds
 * the block (`answerHolds`). The references are the block in each document of the issue
 * (`blockReferences`), none where the issue has none. Undefined for every other address, and
 * while a read the view draws from is out, refused or has a page left, so a block a read holds,
 * or may hold, lands as it does today.
 */
export function unqualifiedAsk({
  address,
  hash,
  held,
  issue,
  others,
  route,
}: {
  readonly address: Pick<IssueAddress, "target" | "version">;
  readonly hash: string;
  /** The reads the address's view draws from, each with every page of it read so far, the
   *  outline read first: that read alone on the issue's own address and Graph's, and the
   *  filter's own view read beside it on the Plan and Verification filters'. Each undefined
   *  while it holds no answer. */
  readonly held: readonly [TreeAnswer | undefined, ...(TreeAnswer | undefined)[]];
  readonly issue: Parameters<typeof blockReferences>[0]["issue"];
  /** Every other read of the tree the page holds: each filter's view answer, and Graph's read. */
  readonly others: readonly (TreeAnswer | undefined)[];
  readonly route: IssueRoute;
}): readonly string[] | undefined {
  const target = address.target;
  if (
    UNQUALIFIED_ROUTES[route.kind] !== true ||
    !hash.startsWith("#b-") ||
    target?.kind !== "block" ||
    target.document !== undefined ||
    address.version !== undefined
  ) {
    return undefined;
  }
  if (held.some((answer) => answer === undefined || nextTreeCursor(answer) !== undefined)) {
    return undefined;
  }
  if ([...held, ...others].some((answer) => answerHolds(answer, target))) return undefined;
  return blockReferences({ block: target.id, issue, key: route.key, tree: held[0]?.tree });
}

/**
 * The one version the members `targets` of a `reference-targets` read last held the block in,
 * at the page's own address of it: each member of a reference in `asked` that answers its block
 * gone with a version the page opens (`lastHeldOf`, `lastHeldPath`). Undefined where none does,
 * and where they open more than one address: the page does not choose between them.
 */
export function lastHeldDestination(
  targets: readonly ReferenceTarget[],
  asked: readonly string[]
): string | undefined {
  const opened = new Set<string>();
  for (const member of targets) {
    const path = lastHeldPath(asked.includes(member.ref) ? lastHeldOf(member) : undefined);
    if (path !== undefined) opened.add(path);
  }
  return opened.size === 1 ? [...opened][0] : undefined;
}

/** Where an address opens instead that names, by a `#b-` fragment of no document, a block no read
 *  holds (`useUnqualifiedLastHeld`), and whether the read that tells is out. */
export interface UnqualifiedOpening {
  /** The address the page opens in place of the address; undefined where it stands. */
  readonly opened: string | undefined;
  /** Whether the one `reference-targets` read of the block is out, which the outline's landing
   *  and its line that no row stands for the block wait on. */
  readonly out: boolean;
}

/** The address stands, and no read of it is out. */
const STANDS: UnqualifiedOpening = { opened: undefined, out: false };

/** The address stands while its read is out. */
const ASKING: UnqualifiedOpening = { opened: undefined, out: true };

/**
 * Where an address on the issue's own address, Graph's, or the Plan or Verification filter's
 * opens whose `#b-` fragment names a block no read holds (R9 and R12 of Graph editing, the
 * architect's decision on plan task t12), once the outline would say in its place that no row
 * stands for it (`absent`): `useAddressFallback`'s `absent` for the block, with every page of its
 * reads read (`useReadUntilHeld`), and no view the page holds drawing the block (`addressView`).
 *
 * The page asks one `reference-targets` read of the block in each document of the issue
 * (`unqualifiedAsk`) and opens the one version its own answer names (`lastHeldDestination`),
 * read once that read has settled, never the names the page published from another read. Where
 * the answer names none, or versions of more than one address, and where the issue holds no
 * document to ask of, the address opens without its fragment, with no line. While the read is out
 * the address stands (`out`); where it is refused, the address stands as today, with the line.
 * The address it opens pins a version or names no block, and asks nothing more.
 */
export function useUnqualifiedLastHeld({
  absent,
  address,
  issue,
  others,
  outline,
  route,
}: {
  /** Whether the outline would say in its place that no row stands for the address's block. */
  readonly absent: boolean;
  readonly address: Pick<IssueAddress, "target" | "version">;
  readonly issue: Parameters<typeof blockReferences>[0]["issue"];
  /** Every other read of the tree the page holds (`unqualifiedAsk`). */
  readonly others: readonly (TreeAnswer | undefined)[];
  /** The outline read the body draws, every page of it read so far (`useTreeRead`). */
  readonly outline: TreeAnswer | undefined;
  readonly route: IssueRoute;
}): UnqualifiedOpening {
  const { hash, pathname, search } = useLocation();
  // On the Plan and Verification filters' addresses the filter's own view read is one the address
  // is looked up in too, watched under the key `ViewReadProvider` reads it by and never sent.
  const filter = route.kind === "plan" || route.kind === "verification" ? route.kind : undefined;
  const view = useQuery({
    ...viewQuery(route.key, filter ?? TREE_FILTERS[0]),
    enabled: false,
  }).data;
  const asked = absent
    ? unqualifiedAsk({
        address,
        hash,
        held: filter === undefined ? [outline] : [outline, view],
        issue,
        others,
        route,
      })
    : undefined;
  const lookup = useQuery({
    ...referenceTargetsQuery(asked ?? []),
    enabled: asked !== undefined && asked.length > 0,
  });
  if (asked === undefined) return STANDS;
  const unblocked = `${pathname}${search}`;
  if (asked.length === 0) return { opened: unblocked, out: false };
  if (lookup.isError) return STANDS;
  if (!lookup.isSuccess || lookup.fetchStatus !== "idle") return ASKING;
  return { opened: lastHeldDestination(lookup.data.targets, asked) ?? unblocked, out: false };
}
