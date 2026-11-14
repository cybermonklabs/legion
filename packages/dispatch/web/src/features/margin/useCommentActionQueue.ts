import { useRef, useState } from "react";

import {
  api,
  apiErrorCodeIn,
  apiErrorMessage,
  type WriteBounds,
  type WriteMethod,
} from "../../api/client";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { stoppedWriteNote } from "../writes/keyed-write";
import { endedStopped, useKeyedWrite } from "../writes/useKeyedWrite";

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

/** What the card says of `action`'s failed press, of the comment `id`. One the reader stopped may
 *  have been saved, and the card's Retry makes it again under its key (`retryItem`). */
function actionFailure(
  id: string,
  action: { readonly error: Error; readonly method: WriteMethod | undefined }
): CommentActionFailure {
  return {
    id,
    message: endedStopped(action)
      ? stoppedWriteNote("this action", "saved", {
          again: "Retry",
          method: action.method,
          stopped: action.error,
        })
      : apiErrorMessage(action.error, "Could not save this action."),
    retryable: !apiErrorCodeIn(action.error, permanentRefusals),
  };
}

interface CommentActionQueueOptions<TContext> {
  onError?(error: Error, input: CommentActionInput, context: TContext | undefined): void;
  onMutate?(input: CommentActionInput): TContext | Promise<TContext>;
  /** The server took `input`; the next queued action starts after this. */
  onSuccess?(input: CommentActionInput): void;
}

/** What the card that offered `kind` calls it, for the shell's indicator. */
const ACTION_LABELS: Record<CommentAction, string> = {
  accept: "Suggestion accepted",
  reject: "Suggestion rejected",
  reopen: "Comment reopened",
  resolve: "Comment resolved",
};

function submitCommentAction({ id, kind }: CommentActionInput, bounds: WriteBounds) {
  if (kind === "accept") return api.acceptComment(id, bounds);
  if (kind === "reject") return api.rejectComment(id, bounds);
  if (kind === "reopen") return api.reopenComment(id, bounds);
  return api.resolveComment(id, bounds);
}

/**
 * Serializes comment state changes everywhere comments render. A second click on the same action
 * is ignored; a click on another thread is retained until the current request settles. A failure
 * clears that pending work so the reader can correct or retry the failed action explicitly.
 */
export function useCommentActionQueue<TContext = undefined>({
  onError,
  onMutate,
  onSuccess,
}: CommentActionQueueOptions<TContext>) {
  const actionGuard = useSubmitGuard();
  const inFlight = useRef<CommentActionInput | undefined>(undefined);
  const queued = useRef(new Map<string, CommentAction>());
  const [queuedIds, setQueuedIds] = useState<ReadonlySet<string>>(() => new Set());
  const syncQueuedIds = () => setQueuedIds(new Set(queued.current.keys()));
  const action = useKeyedWrite<unknown, CommentActionInput, TContext>({
    // The card that offered the action shows its refusal (`CommentActionFailure`).
    inline: true,
    label: ({ kind }) => ACTION_LABELS[kind],
    // Every action is a `POST` of its own route (`submitCommentAction`).
    method: "POST",
    write: submitCommentAction,
    onError,
    onMutate,
    onSettled: (_data, error, input) => {
      actionGuard.release();
      inFlight.current = undefined;
      if (error !== null) {
        queued.current.clear();
        syncQueuedIds();
        return;
      }
      onSuccess?.(input);
      const next = queued.current.entries().next();
      if (next.done) return;
      const [id, kind] = next.value;
      queued.current.delete(id);
      syncQueuedIds();
      start({ id, kind });
    },
  });
  /** Makes `press` under the guard, with `input` the action in flight: a new press of `input`, or
   *  the card's Retry of the newest press (`retry`), which goes under that press's key where the
   *  reader stopped it. */
  function start(input: CommentActionInput, press = () => action.mutate(input)): boolean {
    return actionGuard.guard(() => {
      inFlight.current = input;
      press();
    });
  }

  const activeId = action.isPending ? action.variables?.id : undefined;
  const pendingActionIds =
    activeId === undefined ? queuedIds : new Set<string>([activeId, ...queuedIds]);

  return {
    actionFailure:
      action.isError && action.variables !== undefined
        ? actionFailure(action.variables.id, action)
        : undefined,
    mutateItem: (input: CommentActionInput) => {
      if (start(input)) return;
      const current = inFlight.current;
      if (current !== undefined && current.id === input.id && current.kind === input.kind) return;
      queued.current.set(input.id, input.kind);
      syncQueuedIds();
    },
    pendingActionIds,
    retryItem: () => {
      if (action.variables !== undefined) start(action.variables, action.retry);
    },
  };
}
