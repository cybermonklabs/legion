import { useQueries, useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { api } from "../../api/client";
import {
  artifactBlocksQuery,
  issueCoverageQuery,
  issueResultsQuery,
  referenceTargetsQuery,
} from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { usePublishReferenceNames } from "../refs/reference-names";
import {
  buildIssueTree,
  type DocumentBlocks,
  type IssueTreeReads,
  type TreeNode,
  treeReferences,
} from "./issue-tree";

/**
 * The issue's tree, from the page's reads (dispatch://INFRA-1196 R20). This hook is where the
 * reads are; `buildIssueTree` is pure and holds none of them, so the model is testable without a
 * server and the page makes no request per node.
 *
 * The reads are the issue's own (handed in, because the page already has it), the coverage, the
 * decisions, the results, the references (`?own=1`, which names the blocks citing each document
 * of the issue), the issue's asks, and the blocks of each document - ids, types and attributes,
 * never text, which is how a `serves` edge is held without reading a document the reader has
 * not opened (R20). Every one of them is a key another surface of this issue already reads, so
 * opening the Conversation or the Decisions panel after the tree finds them answered rather
 * than fetching again. The conversation's comments and messages are nodes of this tree too, and
 * a child issue's check reads them (`tree-drafts.ts`, `treeDrafts`). A document's own text is
 * read by the node that opens it and by nothing else (`NodeArtifact.tsx`).
 *
 * The names come last and all at once: the tree is built, every address it holds is read in one
 * batched request (`referenceTargetsQuery`), and the tree is built again with the names in it.
 * Until that answers the tree draws, with a chip naming its target's address and a block of a
 * document carrying no words yet.
 */
export function useIssueTree(issue: IssueDetails): TreeNode {
  const key = issue.key;
  const coverage = useQuery(issueCoverageQuery(key));
  const decisions = useQuery(issueDecisionsQuery(key, true));
  const results = useQuery(issueResultsQuery(key));
  // `?own=1`, so the closure answers the issue's own documents too, each with the blocks of
  // its citer: that is how a document is drawn under the block citing it (`tree-drafts.ts`'s
  // `citations`) without reading any document's stored text, which R20 forbids on a first load.
  const references = useQuery({
    queryKey: ["issue", key, "references", "own"],
    queryFn: () => api.getIssueReferences(key, { own: true }),
  });
  const asks = useQuery({
    queryKey: ["asks", key],
    queryFn: () => api.listIssueAsks(key, "all"),
  });
  // Under the keys the Conversation and the margin already own, so a reader opening the panel
  // after the tree finds them answered rather than read a second time.
  const comments = useQuery({
    queryKey: ["comments", key],
    queryFn: () => api.listComments(key),
  });
  const messages = useQuery({
    queryKey: ["issue-messages", key],
    queryFn: () => api.listIssueMessages(key),
  });
  const documentIds = (issue.artifacts ?? [])
    .filter((artifact) => artifact.kind === "doc")
    .map((artifact) => artifact.id);
  // `combine` is what keeps one value for every document's blocks: TanStack holds its answer and
  // replaces it only when a document's own read changes, where a map over the results would be a
  // new array every render and build the tree again with it. It answers whether any of them is
  // still waiting too, which is part of what holds the one batched name read back below.
  const documents = useQueries({
    combine: (answers): { blocks: DocumentBlocks[]; pending: boolean } => ({
      blocks: answers.map((answer, index) => ({
        artifactId: documentIds[index] ?? "",
        blocks: answer.data ?? [],
      })),
      pending: answers.some((answer) => answer.isPending),
    }),
    queries: documentIds.map((id) => artifactBlocksQuery(id)),
  });
  // One value, so the model is given the reads and nothing else.
  const reads = useMemo<IssueTreeReads>(
    () => ({
      asks: asks.data,
      comments: comments.data,
      coverage: coverage.data,
      decisions: decisions.data,
      documents: documents.blocks,
      issue,
      messages: messages.data,
      references: references.data,
      results: results.data,
    }),
    [
      asks.data,
      comments.data,
      coverage.data,
      decisions.data,
      documents.blocks,
      issue,
      messages.data,
      references.data,
      results.data,
    ]
  );
  const unnamed = useMemo(() => buildIssueTree(reads), [reads]);
  const refs = useMemo(() => treeReferences(unnamed), [unnamed]);
  // One request for every name, not one per read that landed. Each of these reads adds nodes to
  // the tree as it arrives, so asking while any is still out would ask for the addresses known
  // so far and ask again for the rest: measured on this pod, that cost the first load two reads
  // and sometimes three, where the design is "the names come last and all at once" (above) and
  // the issue's own budget is 40 requests (dispatch://INFRA-1196 c-requests). Every one of these
  // reads is always enabled, so each ends in an answer or an error and none of them holds the
  // names for good. Nothing waits on the names: until they land the tree draws with each chip
  // naming its target's address.
  const treeRead =
    !asks.isPending &&
    !comments.isPending &&
    !coverage.isPending &&
    !decisions.isPending &&
    !messages.isPending &&
    !references.isPending &&
    !results.isPending &&
    !documents.pending;
  const targets = useQuery(referenceTargetsQuery(treeRead ? refs : []));
  const named = targets.data === undefined ? undefined : (targets.data.targets ?? []);
  // The same answer every link of this page would otherwise read one pointer at a time: a
  // block's own words, which a link in a document, in a decision's card or in a comment's
  // preview shows (`features/refs/reference-names.tsx`, R20).
  const names = useMemo(
    () =>
      new Map(
        (named ?? [])
          .filter((target) => !target.gone && target.name !== null)
          .map((target) => [target.ref, target.name ?? ""])
      ),
    [named]
  );
  usePublishReferenceNames(names);
  return useMemo(
    () => (named === undefined ? unnamed : buildIssueTree({ ...reads, targets: named })),
    [named, reads, unnamed]
  );
}
