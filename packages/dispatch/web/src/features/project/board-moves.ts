import { useQueryClient } from "@tanstack/react-query";
import { useCallback, useState } from "react";

import { ApiError, issuePatchRequest } from "../../api/client";
import { projectIssuesQueryKey } from "../../api/queries";
import type { IssueSummary, UpdateIssueInput } from "../../api/types";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { type FromPress, useWrites } from "../writes/write-store";
import { type IssueStatus, moveIssue, moveIssueToLane, type PriorityLane } from "./board-model";

export const staleBoardMessage =
  "The board changed while you were moving this card - refreshed, try again.";

const everyIssue = () => true;

/** Where a card was dropped: into a column of the plain board, or into one lane's cell of a
 *  board grouped into swimlanes, at a place counted among the cards the board renders. */
type Drop =
  | { readonly kind: "column"; readonly status: IssueStatus; readonly index: number }
  | {
      readonly kind: "lane";
      readonly status: IssueStatus;
      readonly lane: PriorityLane;
      readonly index: number;
    };

/** One move as its write holds it: the card, the drop's placement the board composed from the
 *  press - which the write takes over as it joins the store - and the PATCH body naming the
 *  neighbours it was dropped between. */
interface BoardMove {
  readonly pressed: FromPress;
  readonly input: UpdateIssueInput;
  readonly key: string;
}

/** `key` placed where `drop` put it among `issues`: the list that placement leaves and the PATCH
 *  body naming the visible neighbours, or undefined when the card is not there or would not
 *  move. */
function placeDrop(
  issues: readonly IssueSummary[],
  key: string,
  drop: Drop,
  isVisible: (issue: IssueSummary) => boolean
): { issues: IssueSummary[]; input: UpdateIssueInput } | undefined {
  return drop.kind === "lane"
    ? moveIssueToLane(issues, key, drop.status, drop.lane, drop.index, isVisible)
    : moveIssue(issues, key, drop.status, drop.index, isVisible);
}

/**
 * The one path every board move takes - pointer, touch and keyboard drops alike. `moveCard`
 * places the card where it was dropped, sends the PATCH naming the visible neighbours, and
 * refetches the project list once the server has answered. The placement is the write's own
 * change: the drop's rule run against the list as it stands, composed from the press
 * (`Writes.fromPress`) over each answer that lands on that list while the move is unanswered (R7
 * of dispatch://INFRA-299) - the card keeps the place the person dropped it at and every other
 * card's move the answer carries arrives with it. Moves on one board are serialised through a
 * single mutation scope, so a second drop computes its neighbours from the board as the person
 * sees it, the first drop's placement composed in (`Writes.shown`), and never overtakes it on the
 * wire. A failed move stops being composed - the board is Dispatch's list with every other live
 * move over it (R10) - and the list is read again, and the board says so: a stale rank
 * (`RANK_INPUT`, the neighbours moved under the drag) gets `staleBoardMessage`; anything else the
 * server's message.
 *
 * With filters active, `labels` names the query the board actually renders (the same key the
 * List shares), so the drop, its leaving and refetch hit the list on screen; a successful move
 * also refetches the unfiltered list, whose rank changed too. `isVisible` is the board's filter
 * predicate: insertion indices count visible cards, the PATCH names visible neighbours, and hidden
 * issues keep their cache positions.
 */
export function useBoardMoves(
  project: string,
  labels: readonly string[] = [],
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
  const writes = useWrites();
  const [error, setError] = useState<string>();
  const queryKey = projectIssuesQueryKey(project, labels);
  // The board says why a move was refused, over the card the store no longer composes moved. A
  // move is one write to its issue, so it goes in press order with every other write to that
  // issue (`issue:<KEY>`) - the header's status, its fields, a components save.
  const patch = useKeyedWrite({
    inline: true,
    label: ({ key }: BoardMove) => `${key} moved on the board`,
    target: ({ key }) => `issue:${key}`,
    request: ({ input, key }) => issuePatchRequest(key, input),
    scope: { id: `board:${project}` },
    /** The drop, composed from its press (`commit`) and taken over by the write as it joins. */
    shows: ({ pressed }) => pressed,
  });
  const { mutateAsync } = patch;
  /** Commits one drop: the card placed where it was dropped, the PATCH, the refusal said, refetch
   *  once it settles - the one tail `moveCard` and `moveCardToLane` share. */
  const commit = useCallback(
    async (key: string, drop: Drop) => {
      // The board as the person sees it: every earlier drop still out composed over Dispatch's.
      const previous = writes.shown<IssueSummary[]>(queryKey);
      if (previous === undefined) {
        return;
      }
      const moved = placeDrop(previous, key, drop, isVisible);
      if (moved === undefined) {
        return;
      }
      // The card must be where it was dropped in this tick - the board's own scope holds a second
      // drop's write until the first answers - so the drop is composed from here
      // (`Writes.fromPress`), in the press's place among the board's live changes: over every
      // earlier drop still out, as it was placed, and over every answer that lands before its
      // write joins. A read already out was asked before this press; the refetch below reads the
      // board again.
      void queryClient.cancelQueries({ queryKey, exact: true });
      const pressed = writes.fromPress((shown) => {
        shown.on<IssueSummary[]>(
          queryKey,
          (current) => placeDrop(current, key, drop, isVisible)?.issues ?? current
        );
      });
      try {
        await mutateAsync({ input: moved.input, key, pressed });
        setError(undefined);
        if (labels.length > 0) {
          await queryClient.invalidateQueries({ queryKey: projectIssuesQueryKey(project, []) });
        }
      } catch (cause) {
        setError(
          cause instanceof ApiError
            ? cause.code === "RANK_INPUT"
              ? staleBoardMessage
              : cause.message
            : "Could not move the card."
        );
      } finally {
        pressed.release();
        await queryClient.invalidateQueries({ queryKey });
      }
    },
    [isVisible, labels, mutateAsync, project, queryClient, queryKey, writes]
  );
  const moveCard = useCallback(
    (key: string, targetStatus: IssueStatus, insertionIndex: number) =>
      commit(key, { index: insertionIndex, kind: "column", status: targetStatus }),
    [commit]
  );
  const moveCardToLane = useCallback(
    (key: string, targetStatus: IssueStatus, targetLane: PriorityLane, insertionIndex: number) =>
      commit(key, {
        index: insertionIndex,
        kind: "lane",
        lane: targetLane,
        status: targetStatus,
      }),
    [commit]
  );
  return { error, moveCard, moveCardToLane };
}
