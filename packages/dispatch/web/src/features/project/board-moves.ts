import { type QueryClient, useMutation, useQueryClient } from "@tanstack/react-query";
import { useCallback, useState } from "react";

import { ApiError, issuePatchRequest } from "../../api/client";
import { boardMoveMutationKey, readIssueList } from "../../api/queries";
import type { Issue, IssueSummary, StatusChangeAnswer, UpdateIssueInput } from "../../api/types";
import {
  cardHasParentOrOpenChild,
  isParentStatusConflict,
  type ParentStatusQuestion,
  useParentStatusMove,
} from "../issue/parent-status/useParentStatusMove";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import type { BoardChange } from "./board-model";
import { projectIssuesQueryKey } from "./issue-filters";

/** The issue as a move leaves it on the page until the server answers: its new status, its new
 *  priority and a rank between its new neighbours, where the move writes each. */
function movedIssue(issue: IssueSummary, { input, rank }: BoardChange): IssueSummary {
  return {
    ...issue,
    ...(typeof input.status === "string" ? { status: input.status } : {}),
    ...(input.priority === undefined ? {} : { priority: input.priority }),
    ...(rank === undefined ? {} : { rank }),
  };
}

/** What one board move sends: the issue (`key`) and its change (`input`). */
export interface BoardMoveVariables {
  readonly input: UpdateIssueInput;
  readonly key: string;
}

/** A move as the Board plans it: its change, or a function that plans it once a confirmed status
 *  change is known (`null` while none is). A move of a card whose status change moves other issues
 *  with it - a parent and its open children - is planned again with those issues in their new
 *  cells, so the card's rank names the neighbours Dispatch now holds. A function answers
 *  undefined where the card would not move. */
type BoardMovePlan = BoardChange | ((answer: StatusChangeAnswer | null) => BoardChange | undefined);

/** One PATCH of a move, made by a keyed write of the Board's. */
type MoveWrite = (variables: BoardMoveVariables) => Promise<Issue | null>;

/** Told each time a move places its card: in the call itself, before its PATCH goes out, for a
 *  card the Board knows no parent or open child of (`cardHasParentOrOpenChild`), and at once for a
 *  move that leaves the card where it is; once the rule's dry run answered that it asks nothing;
 *  and once a choice of the rule's question landed, with each other issue that choice moved
 *  already in its cell. A move that ends with its card not placed - kept, cancelled, or refused
 *  before it was placed - does not tell it. A keyboard move says then where the card landed. */
type OnPlaced = () => void;

/** What the Board says of the move of `key` that did not land. One the reader stopped may have
 *  been made: the listings go back to where the page last read them, and the read that follows
 *  shows where Dispatch holds the card. */
function moveFailure(cause: unknown, key: string): string {
  if (cause instanceof SendStoppedError) {
    return stoppedWriteNote(`the move of ${key}`, "made", {
      anew: "The board is read again from Dispatch, and moving the card again sends a new move.",
    });
  }
  return cause instanceof ApiError ? cause.message : "Could not move the card.";
}

/** The change `plan` makes, once `answer` is known. */
function planned(plan: BoardMovePlan, answer: StatusChangeAnswer | null): BoardChange | undefined {
  return typeof plan === "function" ? plan(answer) : plan;
}

/** A listing query under the project's issues that holds one of the issues a move touches: its
 *  query key and the issues it holds. The page reads and the counts under the same prefix hold no
 *  such list, and no card. */
interface HeldListing {
  readonly queryKey: readonly unknown[];
  readonly issues: readonly IssueSummary[];
}

/** Every listing the views read that holds one of `keys`: the open listing and the reads of done
 *  issues, under `projectIssuesQueryKey(project)`. */
function listingsHolding(
  client: QueryClient,
  project: string,
  keys: ReadonlySet<string>
): HeldListing[] {
  return client
    .getQueriesData<unknown>({ queryKey: projectIssuesQueryKey(project) })
    .flatMap(([queryKey, data]) => {
      const read = readIssueList(data);
      return read?.kind === "listing" && read.issues.some((issue) => keys.has(issue.key))
        ? [{ queryKey, issues: read.issues }]
        : [];
    });
}

/**
 * The one path every Board move takes - pointer, touch and keyboard drops alike. `move` places
 * the card on the page at once, in every listing the views read that holds it (the open listing
 * and the reads of done issues, under `projectIssuesQueryKey(project)`), sends the PATCH the
 * Board's rules planned (`planMove`), and reads the listings again once the server has answered.
 * Moves on one Board are serialised through a single mutation scope, so a second drop is planned
 * from the first one's placement and never overtakes it on the wire. A listing the server
 * answered before the move never lands over the placement: a fetch already on the wire is
 * cancelled as the move writes, and one that starts while a move is unanswered waits for the
 * answer (`projectIssuesQuery`, keyed on `boardMoveMutationKey`). Any failure puts the listings
 * back, reads them again, and says so with the server's message.
 *
 * A move of a card's status that Dispatch's rule could refuse - the card has a parent or an open
 * child (`cardHasParentOrOpenChild`) - asks first (`useParentStatusMove`). It takes its turn in
 * the same scope, so a later drop waits behind it, and the card stays where it is until the
 * person confirms. Where the dry run answers that the rule asks nothing, the move goes as above.
 * Where the rule would refuse it, the Board asks the question (`question`), and a choice confirmed
 * sends the status change with that option, then a PATCH of the card's place (`rank`, and
 * `priority` for a lane move), and only then is the card placed. That PATCH's refusal leaves the
 * status as Dispatch changed it. A move of a card the Board knew no parent or open child of that
 * Dispatch refuses under the rule asks the same question. A move tells its caller each time it
 * places the card (`OnPlaced`).
 */
export function useBoardMoves(project: string): {
  error: string | undefined;
  move: (key: string, plan: BoardMovePlan, onPlaced?: OnPlaced) => Promise<void>;
  /** The question a move of a card's status asks while the rule would refuse it. */
  question: ParentStatusQuestion | undefined;
} {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string>();
  const parentStatus = useParentStatusMove();
  const { ask, move: checkStatus } = parentStatus;
  // The Board says why a move was refused, over the card it rolled back. A move is one write to
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
  // The same write, for a move made inside a check that holds the Board's scope already: one of
  // its own in that scope would wait for the check that waits for it.
  const placed = useKeyedWrite(write);
  // A move the rule could refuse, made as one turn of the Board's scope: its dry run, its
  // question and the writes that follow. It carries the Board's move key, so a read of the
  // listings that starts meanwhile waits for it, as it waits for a PATCH.
  const check = useMutation({
    mutationKey: boardMoveMutationKey(project),
    mutationFn: (turn: () => Promise<void>) => turn(),
    networkMode: "always",
    scope: { id: `board:${project}` },
  });
  const { mutateAsync: patchMove } = patch;
  const { mutateAsync: placedMove } = placed;
  const { mutateAsync: inScope } = check;
  /** Reads the listings again once a move is answered. */
  const refetch = useCallback(
    () => queryClient.invalidateQueries({ queryKey: projectIssuesQueryKey(project) }),
    [project, queryClient]
  );
  /** Places the card `key` as `change` leaves it, in every listing that holds it, and each other
   *  issue `answer` names as the status change left it; tells `onPlaced`; and sends `body`, where
   *  there is one, through `send`. On a refusal puts the listings back and says why - but for the
   *  rule's refusal, which the question that follows says. Answers whether it landed, and the
   *  refusal. */
  const place = useCallback(
    async (
      key: string,
      change: BoardChange,
      body: UpdateIssueInput | undefined,
      send: MoveWrite,
      onPlaced?: OnPlaced,
      answer: StatusChangeAnswer | null = null
    ): Promise<{ landed: boolean; refusal?: unknown }> => {
      const others = new Map(
        (answer?.changed ?? []).filter((entry) => entry.key !== key).map((e) => [e.key, e])
      );
      const held = listingsHolding(queryClient, project, new Set([key, ...others.keys()]));
      for (const { queryKey, issues } of held) {
        // Not awaited: the cancel takes effect synchronously, and the write must land in this
        // tick so a second move is planned from it. The read after it restores what it drops.
        void queryClient.cancelQueries({ queryKey, exact: true });
        queryClient.setQueryData(
          queryKey,
          issues.map((issue) => {
            if (issue.key === key) return movedIssue(issue, change);
            const entry = others.get(issue.key);
            return entry === undefined
              ? issue
              : { ...issue, parent: entry.parent, status: entry.status };
          })
        );
      }
      onPlaced?.();
      if (body === undefined) {
        setError(undefined);
        return { landed: true };
      }
      try {
        await send({ input: body, key });
        setError(undefined);
        return { landed: true };
      } catch (cause) {
        for (const { queryKey, issues } of held) queryClient.setQueryData(queryKey, issues);
        if (!isParentStatusConflict(cause)) setError(moveFailure(cause, key));
        return { landed: false, refusal: cause };
      }
    },
    [project, queryClient]
  );
  /** Once a chosen status change landed: the card's place - its `rank`, and its `priority` for a
   *  lane move - as a PATCH of its own, with the status taken out, and none where the move writes
   *  neither. The move is planned again with each other issue the change moved (`answer`) in its
   *  new cell, and those issues are put there first: the PATCH then names the card's neighbour as
   *  Dispatch now holds the cell, so the read that follows leaves the card where it is drawn, and
   *  `onPlaced` reads the cell with every issue the change moved. Waiting for that read instead
   *  would leave those issues in their old cells until it lands, and place the card among
   *  neighbours that lack them. Its refusal leaves the status as Dispatch changed it. */
  const placeAfterChange = useCallback(
    async (
      key: string,
      plan: BoardMovePlan,
      answer: StatusChangeAnswer | null,
      onPlaced?: OnPlaced
    ) => {
      const change = planned(plan, answer);
      if (change === undefined) return;
      const { status: _status, ...rest } = change.input;
      const body = Object.keys(rest).length === 0 ? undefined : rest;
      await place(key, change, body, placedMove, onPlaced, answer);
    },
    [place, placedMove]
  );
  /** The question for a move Dispatch refused under the rule, and the card's place once a
   *  choice landed. */
  const asked = useCallback(
    async (
      key: string,
      status: string,
      plan: BoardMovePlan,
      conflict: ApiError,
      onPlaced?: OnPlaced
    ) => {
      const end = await ask({ key, status }, conflict);
      if (end.kind === "changed") await placeAfterChange(key, plan, end.answer, onPlaced);
    },
    [ask, placeAfterChange]
  );
  /** A move of the status of a card with a parent or an open child, inside the Board's scope. No
   *  read of the listings is awaited in it: they wait for the turn this runs in. */
  const checked = useCallback(
    async (key: string, status: string, plan: BoardMovePlan, onPlaced?: OnPlaced) => {
      const end = await checkStatus({
        key,
        patch: async () => {
          // Planned again: the Board may have changed while the dry run was out.
          const change = planned(plan, null);
          if (change === undefined) {
            onPlaced?.();
            return;
          }
          const { refusal } = await place(key, change, change.input, placedMove, onPlaced);
          if (isParentStatusConflict(refusal)) await asked(key, status, plan, refusal, onPlaced);
        },
        related: true,
        status,
      });
      if (end.kind === "refused") setError(moveFailure(end.refusal, key));
      if (end.kind === "changed") await placeAfterChange(key, plan, end.answer, onPlaced);
    },
    [asked, checkStatus, place, placeAfterChange, placedMove]
  );
  const move = useCallback(
    async (key: string, plan: BoardMovePlan, onPlaced?: OnPlaced) => {
      const change = planned(plan, null);
      if (change === undefined) {
        onPlaced?.();
        return;
      }
      // The status the move writes, where it moves the card to another column.
      const status = typeof change.input.status === "string" ? change.input.status : undefined;
      const card = listingsHolding(queryClient, project, new Set([key]))
        .flatMap(({ issues }) => issues)
        .find((issue) => issue.key === key);
      if (status !== undefined && card !== undefined && cardHasParentOrOpenChild(card)) {
        await inScope(() => checked(key, status, plan, onPlaced));
        await refetch();
        return;
      }
      const { refusal } = await place(key, change, change.input, patchMove, onPlaced);
      await refetch();
      if (status !== undefined && isParentStatusConflict(refusal)) {
        await inScope(() => asked(key, status, plan, refusal, onPlaced));
        await refetch();
      }
    },
    [asked, checked, inScope, patchMove, place, project, queryClient, refetch]
  );
  return { error, move, question: parentStatus.question };
}
