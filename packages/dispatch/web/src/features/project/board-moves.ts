import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useCallback, useState } from "react";

import { ApiError, issuePatchRequest } from "../../api/client";
import { boardMoveMutationKey } from "../../api/queries";
import type { Issue, IssueSummary, StatusChangeAnswer, UpdateIssueInput } from "../../api/types";
import {
  cardHasParentOrOpenChild,
  isParentStatusConflict,
  type ParentStatusQuestion,
  useParentStatusMove,
} from "../issue/parent-status/useParentStatusMove";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { type IssueStatus, moveIssue, moveIssueToLane, type PriorityLane } from "./board-model";
import { projectIssuesQueryKey } from "./issue-filters";

export const staleBoardMessage =
  "The board changed while you were moving this card - refreshed, try again.";

const everyIssue = () => true;

/** What one board move sends: the issue (`key`) and its change (`input`). */
export interface BoardMoveVariables {
  readonly input: UpdateIssueInput;
  readonly key: string;
}

/** A move as the board makes it of the list it reads: the list with the card placed, and the
 *  change the PATCH sends; undefined when the card is unknown or would not move. */
type MovePlan = (
  issues: readonly IssueSummary[]
) => { issues: IssueSummary[]; input: UpdateIssueInput } | undefined;

/** One PATCH of a move, made by a keyed write of the board's. */
type MoveWrite = (variables: BoardMoveVariables) => Promise<Issue | null>;

/** Told when a move places its card later than the call that made it: once the rule's dry run
 *  answered that it asks nothing, or once a choice of the rule's question landed, with each other
 *  issue that choice moved already in its cell. A move placed at once, in the call itself, does
 *  not tell it: its caller reads the place when the call returns. A keyboard move says where the
 *  card landed then, as it says it at once otherwise. */
type OnPlaced = () => void;

/** What the board says of the move of `key` that did not land. One the reader stopped may have
 *  been made: the card goes back to where the page last read it, and the read that follows shows
 *  where Dispatch holds it. */
function moveFailure(cause: unknown, key: string): string {
  if (cause instanceof SendStoppedError) {
    return stoppedWriteNote(`the move of ${key}`, "made", {
      anew: "The board is read again from Dispatch, and moving the card again sends a new move.",
    });
  }
  if (cause instanceof ApiError) {
    return cause.code === "RANK_INPUT" ? staleBoardMessage : cause.message;
  }
  return "Could not move the card.";
}

/** `issues` with each issue a status change moved but the card `key` at the status and under the
 *  parent Dispatch answered for it, each where it stands in the list, so the list keeps its rank
 *  order. The card is left to its move's plan, which places it among them. An answer of null, or
 *  one that moved no other issue, leaves the list as it is. */
function withStatusChange(
  issues: readonly IssueSummary[],
  key: string,
  answer: StatusChangeAnswer | null
): readonly IssueSummary[] {
  const moved = new Map(
    (answer?.changed ?? []).filter((entry) => entry.key !== key).map((entry) => [entry.key, entry])
  );
  if (moved.size === 0) return issues;
  return issues.map((issue) => {
    const entry = moved.get(issue.key);
    return entry === undefined ? issue : { ...issue, parent: entry.parent, status: entry.status };
  });
}

/**
 * The one path every board move takes - pointer, touch and keyboard drops alike. `moveCard`
 * places the card optimistically, sends the PATCH naming the visible neighbours, and refetches
 * the project list once the server has answered. Moves on one board are serialised through a
 * single mutation scope, so a second drop computes its neighbours from the first drop's
 * optimistic state and never overtakes it on the wire. A list the server answered before the
 * move never lands over the optimistic placement: a fetch already on the wire is cancelled at the
 * optimistic write, and one that starts while a move is unanswered waits for the answer
 * (`projectIssuesQuery`, keyed on `boardMoveMutationKey`). Any failure rolls the board back,
 * refetches it, and says so: a stale rank (`RANK_INPUT`, the neighbours moved under the drag)
 * gets `staleBoardMessage`; anything else the server's message.
 *
 * A move of a card's status that Dispatch's rule could refuse - the card has a parent or an open
 * child (`cardHasParentOrOpenChild`) - asks first (`useParentStatusMove`). It takes its turn in
 * the same scope, so a later drop waits behind it, and the card stays where it is until the
 * person confirms. Where the dry run answers that the rule asks nothing, the move goes as above.
 * Where the rule would refuse it, the board asks the question (`question`), and a choice confirmed
 * sends the status change with that option, then a PATCH of the card's place (`rank`, and
 * `priority` for a lane move), and only then is the card placed. That PATCH's refusal leaves the
 * status as Dispatch changed it. A move of a card the board knew no parent or open child of that
 * Dispatch refuses under the rule asks the same question. A move placed after its call returned
 * tells its caller once the card is placed (`OnPlaced`).
 *
 * With filters active, `labels` names the query the board actually renders (the same key the
 * List shares), so the optimistic write, rollback and refetch hit the list on screen; a
 * successful move also refetches the unfiltered list, whose rank changed too. `isVisible` is the
 * board's filter predicate: insertion indices count visible cards, the PATCH names visible
 * neighbours, and hidden issues keep their cache positions.
 */
export function useBoardMoves(
  project: string,
  labels: readonly string[] = [],
  isVisible: (issue: IssueSummary) => boolean = everyIssue
): {
  error: string | undefined;
  moveCard: (
    key: string,
    targetStatus: IssueStatus,
    insertionIndex: number,
    onPlaced?: OnPlaced
  ) => Promise<void>;
  /** The lane-aware twin of `moveCard`, for a board grouped into swimlanes: moving a card into
   *  another lane reassigns its priority along with its status and rank. */
  moveCardToLane: (
    key: string,
    targetStatus: IssueStatus,
    targetLane: PriorityLane,
    insertionIndex: number,
    onPlaced?: OnPlaced
  ) => Promise<void>;
  /** The question a move of a card's status asks while the rule would refuse it. */
  question: ParentStatusQuestion | undefined;
} {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string>();
  const parentStatus = useParentStatusMove();
  const { ask, move } = parentStatus;
  // The board says why a move was refused, over the card it rolled back. A move is one write to
  // its issue, so it goes in press order with every other write to that issue (`issue:<KEY>`) -
  // the header's status, its fields, a components save.
  const write = {
    inline: true,
    label: ({ key }: BoardMoveVariables) => `${key} moved on the board`,
    target: ({ key }: BoardMoveVariables) => `issue:${key}`,
    request: ({ input, key }: BoardMoveVariables) => issuePatchRequest(key, input),
    mutationKey: boardMoveMutationKey(project),
  };
  const patch = useKeyedWrite({ ...write, scope: { id: `board:${project}` } });
  // The same write, for a move made inside a check that holds the board's scope already: one of
  // its own in that scope would wait for the check that waits for it.
  const placed = useKeyedWrite(write);
  // A move the rule could refuse, made as one turn of the board's scope: its dry run, its
  // question and the writes that follow. It carries the board's move key, so a read of the list
  // that starts meanwhile waits for it, as it waits for a PATCH.
  const check = useMutation({
    mutationKey: boardMoveMutationKey(project),
    mutationFn: (turn: () => Promise<void>) => turn(),
    networkMode: "always",
    scope: { id: `board:${project}` },
  });
  const { mutateAsync: patchMove } = patch;
  const { mutateAsync: placedMove } = placed;
  const { mutateAsync: inScope } = check;
  const queryKey = projectIssuesQueryKey(project, labels);
  /** Reads the lists again once a move is answered: the unfiltered list too after a labelled
   *  move landed, whose rank changed as well. */
  const refetch = useCallback(
    async (landed: boolean) => {
      if (landed && labels.length > 0) {
        await queryClient.invalidateQueries({ queryKey: projectIssuesQueryKey(project, []) });
      }
      await queryClient.invalidateQueries({ queryKey });
    },
    [labels, project, queryClient, queryKey]
  );
  /** Places the move `plan` makes of the list as it reads now, tells `onPlaced`, and sends its
   *  change through `send`; on a refusal puts the list back and says why - but for the rule's
   *  refusal, which the question that follows says. Answers whether it landed, and the refusal. */
  const place = useCallback(
    async (
      key: string,
      plan: MovePlan,
      send: MoveWrite,
      onPlaced?: OnPlaced
    ): Promise<{ landed: boolean; refusal?: unknown }> => {
      const previous = queryClient.getQueryData<IssueSummary[]>(queryKey);
      const moved = previous === undefined ? undefined : plan(previous);
      if (previous === undefined || moved === undefined) {
        return { landed: false };
      }
      // Not awaited: the cancel takes effect synchronously, and the write must land in this
      // tick so a second move computes from it. The refetch after it restores what it drops.
      void queryClient.cancelQueries({ queryKey, exact: true });
      queryClient.setQueryData(queryKey, moved.issues);
      onPlaced?.();
      try {
        await send({ input: moved.input, key });
        setError(undefined);
        return { landed: true };
      } catch (cause) {
        queryClient.setQueryData(queryKey, previous);
        if (!isParentStatusConflict(cause)) setError(moveFailure(cause, key));
        return { landed: false, refusal: cause };
      }
    },
    [queryClient, queryKey]
  );
  /** Once a chosen status change landed: the card's place - its `rank`, and its `priority` for a
   *  lane move - as a PATCH of its own, with the status taken out. The card is placed among each
   *  other issue the change moved (`answer`), put in its new cell first: the PATCH then names the
   *  card's neighbours as Dispatch now holds the cell, so the read that follows leaves the card
   *  where it is drawn, and `onPlaced` reads the cell with every issue the change moved. Waiting
   *  for that read instead would leave those issues in their old cells until it lands, and place
   *  the card among neighbours that lack them. Its refusal leaves the status as Dispatch changed
   *  it. No read here is awaited: the list waits for the check this runs in. */
  const placeAfterChange = useCallback(
    async (key: string, plan: MovePlan, answer: StatusChangeAnswer | null, onPlaced?: OnPlaced) => {
      const { landed } = await place(
        key,
        (issues) => {
          const moved = plan(withStatusChange(issues, key, answer));
          if (moved === undefined) return undefined;
          const { status: _status, ...input } = moved.input;
          return { ...moved, input };
        },
        placedMove,
        onPlaced
      );
      void refetch(landed);
    },
    [place, placedMove, refetch]
  );
  /** The question for a move Dispatch refused under the rule, and the card's place once a
   *  choice landed. */
  const asked = useCallback(
    async (
      key: string,
      status: string,
      plan: MovePlan,
      conflict: ApiError,
      onPlaced?: OnPlaced
    ) => {
      const end = await ask({ key, status }, conflict);
      if (end.kind === "changed") await placeAfterChange(key, plan, end.answer, onPlaced);
    },
    [ask, placeAfterChange]
  );
  /** A move of the status of a card with a parent or an open child, inside the board's scope. */
  const checked = useCallback(
    async (key: string, status: string, plan: MovePlan, onPlaced?: OnPlaced) => {
      const end = await move({
        key,
        patch: async () => {
          const { landed, refusal } = await place(key, plan, placedMove, onPlaced);
          void refetch(landed);
          if (isParentStatusConflict(refusal)) await asked(key, status, plan, refusal, onPlaced);
        },
        related: true,
        status,
      });
      if (end.kind === "refused") setError(moveFailure(end.refusal, key));
      if (end.kind === "changed") await placeAfterChange(key, plan, end.answer, onPlaced);
    },
    [asked, move, place, placeAfterChange, placedMove, refetch]
  );
  /** Commits a move `plan` makes of the list: through the question where the rule could refuse
   *  it, else optimistic write, PATCH, rollback-and-refetch on failure, refetch on success - the
   *  one tail `moveCard` and `moveCardToLane` share. A move placed at once tells `onPlaced`
   *  nothing; one placed later, after the dry run or a choice of the question, tells it then. */
  const commit = useCallback(
    async (key: string, plan: MovePlan, onPlaced?: OnPlaced) => {
      const previous = queryClient.getQueryData<IssueSummary[]>(queryKey);
      const moved = previous === undefined ? undefined : plan(previous);
      if (moved === undefined) {
        return;
      }
      // The status the move writes, where it moves the card to another column.
      const status = moved.input.status ?? undefined;
      const card = previous?.find((issue) => issue.key === key);
      if (status !== undefined && card !== undefined && cardHasParentOrOpenChild(card)) {
        await inScope(() => checked(key, status, plan, onPlaced));
        return;
      }
      const { landed, refusal } = await place(key, () => moved, patchMove);
      await refetch(landed);
      if (status !== undefined && isParentStatusConflict(refusal)) {
        await inScope(() => asked(key, status, plan, refusal, onPlaced));
      }
    },
    [asked, checked, inScope, patchMove, place, queryClient, queryKey, refetch]
  );
  const moveCard = useCallback(
    (key: string, targetStatus: IssueStatus, insertionIndex: number, onPlaced?: OnPlaced) =>
      commit(
        key,
        (issues) => moveIssue(issues, key, targetStatus, insertionIndex, isVisible),
        onPlaced
      ),
    [commit, isVisible]
  );
  const moveCardToLane = useCallback(
    (
      key: string,
      targetStatus: IssueStatus,
      targetLane: PriorityLane,
      insertionIndex: number,
      onPlaced?: OnPlaced
    ) =>
      commit(
        key,
        (issues) =>
          moveIssueToLane(issues, key, targetStatus, targetLane, insertionIndex, isVisible),
        onPlaced
      ),
    [commit, isVisible]
  );
  return { error, moveCard, moveCardToLane, question: parentStatus.question };
}
