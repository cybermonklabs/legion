import { useMemo, useState } from "react";

import {
  api,
  apiErrorCodeIn,
  apiErrorMessage,
  commentAcceptRequest,
  commentRejectRequest,
  commentReopenRequest,
  commentResolveRequest,
} from "../../api/client";
import { newSendKey } from "../../lib/send-key";
import { retriesUnderItsKey } from "../writes/keyed-write";
import { type Shape, shape } from "../writes/write-storage";
import {
  defineWriteKind,
  ofKind,
  useWriteSelection,
  useWrites,
  type Write,
  type Writes,
} from "../writes/write-store";

export type CommentAction = "accept" | "reject" | "resolve" | "reopen";

export interface CommentActionInput {
  id: string;
  kind: CommentAction;
}

/** The comment action that last failed, as the card that offered it shows it. */
export interface CommentActionFailure {
  id: string;
  message: string;
  /** False for a refusal of what the action would write, which the same request never gets past. */
  retryable: boolean;
}

/** Refusals of the document an accept would write: the suggestion itself has to change. */
const permanentRefusals = new Set([
  "CAP_EXCEEDED",
  "INVALID_ASK_BLOCK",
  "INVALID_MARKDOWN",
  "INVALID_OP",
]);

function actionFailure(id: string, error: Error): CommentActionFailure {
  return {
    id,
    message: apiErrorMessage(error, "Could not save this action."),
    retryable: !apiErrorCodeIn(error, permanentRefusals),
  };
}

/** A refused action as the card reports it, with what a Retry makes again: the press itself, and
 *  the key it went out under where Dispatch never gave its verdict (`retriesUnderItsKey`), so a
 *  Retry of a stopped action is that action again rather than a second one. */
interface Refused {
  readonly failure: CommentActionFailure;
  readonly input: CommentActionInput;
  readonly key: string | undefined;
}

interface CommentActionQueueOptions<TContext> {
  onError?(error: Error, input: CommentActionInput, context: TContext | undefined): void;
  onMutate?(input: CommentActionInput): TContext | Promise<TContext>;
  /** The server took `input`; the next action waiting starts after this. */
  onSuccess?(input: CommentActionInput): void;
}

/** What the card that offered `kind` calls it, for the shell's indicator. */
const ACTION_LABELS: Record<CommentAction, string> = {
  accept: "Suggestion accepted",
  reject: "Suggestion rejected",
  reopen: "Comment reopened",
  resolve: "Comment resolved",
};

/** The request one comment action makes: each is a write of that comment's own row, so each
 *  stands in its line (`comment:<id>`). */
function commentActionRequest({ id, kind }: CommentActionInput) {
  if (kind === "accept") return commentAcceptRequest(id);
  if (kind === "reject") return commentRejectRequest(id);
  if (kind === "reopen") return commentReopenRequest(id);
  return commentResolveRequest(id);
}

const keptCommentAction = shape.record({
  id: shape.string,
  kind: shape.oneOf("accept", "reject", "reopen", "resolve"),
}) satisfies Shape<CommentActionInput>;

/**
 * One comment action - a resolve, a reopen, or a suggestion accepted or rejected - as one write
 * of the store, ordered on the comment it writes (`comment:<id>`) and made again from its record
 * after a reload, so an action the person pressed reaches Dispatch though the card that offered
 * it is gone. A reload's action waits for nothing: the actions it stood behind went with the page
 * that pressed them.
 */
export const commentActionWrite = defineWriteKind<CommentActionInput>({
  kept: keptCommentAction,
  name: "comment-action",
  resume: (detail) => (bounds) => api.send(commentActionRequest(detail), bounds),
});

/** An action the engine gave up because an action pressed before it was refused: the card that
 *  offered it shows nothing, as the clicks the engine dropped behind a failure showed nothing. */
class CommentActionClearedError extends Error {
  constructor() {
    super("Dropped behind an action that failed.");
    this.name = "CommentActionClearedError";
  }
}

/** Whether `row` is an action that may still reach the server: waiting for an earlier one, out,
 *  or late. A refused one never is - the card shows its refusal itself (`inline`), so the store
 *  lets it go the moment Dispatch refuses it. */
function stillInLine(row: Write): boolean {
  return row.status === "queued" || row.status === "sending" || row.status === "late";
}

/**
 * Holds one comment action until every action pressed before it has landed, and gives it up
 * (`CommentActionClearedError`) once one of those is refused: an action on another comment waits
 * for the one in flight, and a failure clears the actions still waiting. Both are read from the
 * store's own rows - the press order is the order the store joined them in, and the refusal is
 * the store's own outcome - rather than from a map the component holds, so neither dies with the
 * card that pressed them. Two actions on one comment need none of this: the store's own line
 * (`comment:<id>`) orders them.
 */
function afterEarlierActions(writes: Writes, key: string): Promise<void> {
  /** This action's own row, once the store has joined it: every action of the store is earlier
   *  than one that has none yet. */
  const mine = writes.rows().find((row) => row.key === key)?.id ?? Number.POSITIVE_INFINITY;
  const earlier = (): boolean =>
    writes.rows().some((row) => {
      const action = ofKind(row, commentActionWrite);
      return action !== undefined && action.id < mine && stillInLine(action);
    });
  if (!earlier()) return Promise.resolve();
  const waited = Promise.withResolvers<void>();
  let cleared = false;
  let stopOutcomes = (): void => {};
  let stopRows = (): void => {};
  const stop = (): void => {
    stopOutcomes();
    stopRows();
  };
  stopOutcomes = writes.onOutcome((outcome) => {
    const action = ofKind(outcome.write, commentActionWrite);
    if (outcome.kind === "refused" && action !== undefined && action.id < mine) {
      cleared = true;
    }
  });
  stopRows = writes.subscribe(() => {
    const gone = writes.rows().every((row) => row.key !== key);
    if (!gone && earlier()) return;
    stopRows();
    // The store publishes its rows before it tells of the refusal that changed them, so the
    // verdict is read a task later: by then a refusal of an earlier action has arrived.
    void Promise.resolve().then(() => {
      stop();
      if (cleared || gone) waited.reject(new CommentActionClearedError());
      else waited.resolve();
    });
  });
  return waited.promise;
}

/**
 * Every comment state change the page makes, everywhere comments render: each press is one write
 * of the store on its comment's line (`comment:<id>`), kept with the action it will make, so it
 * reaches Dispatch whatever becomes of the card - another page, an unmount, a reload.
 *
 * Two behaviours of the queue this replaces stay, read from the store's rows rather than from a
 * map of the component's (`afterEarlierActions`): an action on another comment waits for the one
 * in flight, and a failure clears the actions still waiting, which show nothing and are never
 * sent. A second press of an action already in line is refused rather than queued - the card
 * offered one press, and the store holds it - and the card that offered a refused action shows
 * its refusal with a Retry (`CommentActionFailure`).
 */
export function useCommentActionQueue<TContext = undefined>({
  onError,
  onMutate,
  onSuccess,
}: CommentActionQueueOptions<TContext>) {
  const writes = useWrites();
  const [refused, setRefused] = useState<Refused | undefined>(undefined);
  const inLine = useWriteSelection("comment-actions", (rows) =>
    rows.flatMap((row) => {
      const action = ofKind(row, commentActionWrite);
      return action !== undefined && stillInLine(action) ? [action.detail] : [];
    })
  );
  const pendingActionIds = useMemo(() => new Set(inLine.map((action) => action.id)), [inLine]);

  function press(input: CommentActionInput, under: string | undefined): void {
    const key = under ?? newSendKey();
    const request = commentActionRequest(input);
    let context: TContext | undefined;
    let begun = false;
    setRefused(undefined);
    void writes
      .run({
        detail: input,
        // The card that offered the action shows its refusal (`CommentActionFailure`).
        inline: true,
        key,
        kind: commentActionWrite,
        label: ACTION_LABELS[input.kind],
        presentation: "indicator",
        target: `comment:${input.id}`,
        write: async (bounds) => {
          if (!begun) {
            await afterEarlierActions(writes, key);
            // What the card shows while the action is out is written as it goes out, so an
            // action cleared behind a failure never showed anything to put back.
            context = (await onMutate?.(input)) as TContext | undefined;
            begun = true;
          }
          return api.send(request, bounds);
        },
      })
      .then(
        () => {
          onSuccess?.(input);
        },
        (error: unknown) => {
          if (error instanceof CommentActionClearedError) return;
          const refusal = error instanceof Error ? error : new Error(String(error));
          onError?.(refusal, input, context);
          setRefused({
            failure: actionFailure(input.id, refusal),
            input,
            key: retriesUnderItsKey(refusal) ? key : undefined,
          });
        }
      );
  }

  return {
    actionFailure: refused?.failure,
    mutateItem: (input: CommentActionInput) => {
      // A second press of an action already in line is the same action: the store holds it.
      if (inLine.some((action) => action.id === input.id && action.kind === input.kind)) return;
      press(input, undefined);
    },
    pendingActionIds,
    retryItem: () => {
      if (refused !== undefined) press(refused.input, refused.key);
    },
  };
}
