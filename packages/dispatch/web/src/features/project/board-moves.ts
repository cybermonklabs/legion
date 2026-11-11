import { type QueryKey, useQueryClient } from "@tanstack/react-query";
import { useCallback, useState } from "react";

import { ApiError, issuePatchRequest } from "../../api/client";
import { boardMoveMutationKey } from "../../api/queries";
import type { IssueSummary, UpdateIssueInput } from "../../api/types";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { type IssueStatus, moveIssue, moveIssueToLane, type PriorityLane } from "./board-model";

export const staleBoardMessage =
  "The board changed while you were moving this card - refreshed, try again.";

const everyIssue = () => true;

/**
 * The one path every board move takes - pointer, touch and keyboard drops alike. It places the
 * card optimistically in every listing cache that already holds it, sends the PATCH naming the
 * current view's visible neighbours, and refetches those listings when the server answers.
 * Moves on one board are serialised through a single mutation scope, so a second drop computes
 * its neighbours from the first drop's optimistic state and never overtakes it on the wire.
 */
export function useBoardMoves(
  project: string,
  listingKeys: readonly QueryKey[],
  isVisible: (issue: IssueSummary) => boolean = everyIssue
): {
  error: string | undefined;
  moveCard: (key: string, targetStatus: IssueStatus, insertionIndex: number) => Promise<void>;
  /** The lane-aware twin of `moveCard`, for a board grouped into swimlanes: moving a card into
   *  another lane reassigns its priority along with its status and rank. */
  moveCardToLane: (
    key: string,
    targetStatus: IssueStatus,
    targetLane: PriorityLane,
    insertionIndex: number
  ) => Promise<void>;
} {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string>();
  const patch = useKeyedWrite({
    inline: true,
    label: ({ key }: { input: UpdateIssueInput; key: string }) => `${key} moved on the board`,
    target: ({ key }) => `issue:${key}`,
    request: ({ input, key }) => issuePatchRequest(key, input),
    mutationKey: boardMoveMutationKey(project),
    scope: { id: `board:${project}` },
  });
  const { mutateAsync } = patch;
  type Listing = { readonly key: QueryKey; readonly issues: IssueSummary[] };
  const listingsHolding = useCallback(
    (issueKey: string): Listing[] =>
      listingKeys.flatMap((listingKey) => {
        const issues = queryClient.getQueryData<IssueSummary[]>(listingKey);
        return issues?.some((issue) => issue.key === issueKey) ? [{ key: listingKey, issues }] : [];
      }),
    [listingKeys, queryClient]
  );
  /** Commits an already-computed move: writes every observed listing cache, then restores and
   * refetches each one if Dispatch refuses the PATCH. */
  const commit = useCallback(
    async (
      key: string,
      moved: { issues: IssueSummary[]; input: UpdateIssueInput } | undefined,
      primary: Listing,
      listings: readonly Listing[]
    ) => {
      if (moved === undefined) return;
      for (const listing of listings) {
        // The cancellation starts now, but the cache write cannot wait for it: a second drop in
        // this turn must see the first placement when it derives its neighbours.
        void queryClient.cancelQueries({ queryKey: listing.key, exact: true });
        queryClient.setQueryData(
          listing.key,
          listing.key === primary.key
            ? moved.issues
            : listing.issues.map((issue) =>
                issue.key !== key
                  ? issue
                  : {
                      ...issue,
                      ...(moved.input.priority === undefined
                        ? {}
                        : { priority: moved.input.priority }),
                      ...(moved.input.status === undefined ? {} : { status: moved.input.status }),
                    }
              )
        );
      }
      try {
        await mutateAsync({ input: moved.input, key });
        setError(undefined);
      } catch (cause) {
        for (const listing of listings) queryClient.setQueryData(listing.key, listing.issues);
        setError(
          cause instanceof ApiError
            ? cause.code === "RANK_INPUT"
              ? staleBoardMessage
              : cause.message
            : "Could not move the card."
        );
      } finally {
        await Promise.all(
          listings.map((listing) =>
            queryClient.invalidateQueries({ queryKey: listing.key, exact: true })
          )
        );
      }
    },
    [mutateAsync, queryClient]
  );
  const moveCard = useCallback(
    async (key: string, targetStatus: IssueStatus, insertionIndex: number) => {
      const listings = listingsHolding(key);
      const primary = listings[0];
      if (primary === undefined) return;
      await commit(
        key,
        moveIssue(primary.issues, key, targetStatus, insertionIndex, isVisible),
        primary,
        listings
      );
    },
    [commit, isVisible, listingsHolding]
  );
  const moveCardToLane = useCallback(
    async (
      key: string,
      targetStatus: IssueStatus,
      targetLane: PriorityLane,
      insertionIndex: number
    ) => {
      const listings = listingsHolding(key);
      const primary = listings[0];
      if (primary === undefined) return;
      await commit(
        key,
        moveIssueToLane(primary.issues, key, targetStatus, targetLane, insertionIndex, isVisible),
        primary,
        listings
      );
    },
    [commit, isVisible, listingsHolding]
  );
  return { error, moveCard, moveCardToLane };
}
