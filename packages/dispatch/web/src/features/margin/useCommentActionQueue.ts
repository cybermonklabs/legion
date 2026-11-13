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
import { retriesUnderItsKey, SendStoppedError } from "../writes/keyed-write";
import { type Shape, shape } from "../writes/write-storage";
import {
  defineWriteKind,
  liveWrite,
  ofKind,
  useWriteSelection,
  useWrites,
  type WriteChange,
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

/** A refused action as the card reports it, with what a Retry makes again: the action, and the
 *  key it goes under - the refused press's own where Dispatch never gave its verdict
 *  (`retriesUnderItsKey`), so a Retry of a stopped action is that action again rather than a
 *  second one, and none where a Retry is a press of its own. */
interface Refused {
  readonly failure: CommentActionFailure;
  readonly input: CommentActionInput;
  readonly key: string | undefined;
}

interface CommentActionQueueOptions {
  /** Runs as the action goes out, before it shows its change. */
  onMutate?(input: CommentActionInput): void | Promise<void>;
  /** The change this action shows, named as the action goes out and held with the write
   *  (`Writes.shows`), so the store shows it again over each answer that lands on a cache it
   *  wrote until the action settles. */
  shows?(input: CommentActionInput): WriteChange;
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

/** An action the store stopped trying while it waited its turn, before anything of it went out:
 *  held, let go, or stopped by the reader (Stop trying). Nothing was sent, so a Retry under its
 *  key is the action's first request (`retriesUnderItsKey`). */
class CommentActionStoppedError extends SendStoppedError {
  constructor() {
    super("Not sent: this action was stopped before it went out. Retry sends it.");
    this.name = "CommentActionStoppedError";
  }
}

/**
 * The comment actions pressed before one press, watched from that press until it settles
 * (`stop`). It holds one action until every action pressed before it has landed, and gives it up
 * (`CommentActionClearedError`) once one of those is refused: an action on another comment waits
 * for the one in flight, and a failure clears the actions still waiting. All of it is read from
 * the store - the press order is the order the store joined them in, and what became of each is
 * the store's own outcome (`Writes.onOutcome`), told for every way a write leaves it - rather
 * than from a map the component holds, so neither dies with the card that pressed them.
 *
 * The wait (`wait`) is one attempt of the action, so it ends as the store stops trying it
 * (`signal`), with `CommentActionStoppedError`: a hold - its person's sign-out out here or in
 * another tab, their session lapsed - puts the action back in its line to wait again once the
 * hold ends, and a write the store let go - a sign-out, another login taking over - is never
 * sent. What the press watches outlives its attempts: a refusal that came while the action stood
 * held in its line, with no attempt waiting, is kept (`#refused`), so the attempt after the hold
 * reads it and clears the action rather than sending it behind the refused one. Two actions on
 * one comment need none of this: the store's own line (`comment:<id>`) orders them.
 */
class EarlierActions {
  readonly #writes: Writes;
  readonly #key: string;
  /** Whether an action pressed before this one was refused since this press. */
  #refused = false;
  /** Ends the attempt waiting now, if one is, once an action pressed before it leaves. */
  #left: (() => void) | undefined;
  readonly stop: () => void;

  constructor(writes: Writes, key: string) {
    this.#writes = writes;
    this.#key = key;
    this.stop = writes.onOutcome((outcome) => {
      const action = ofKind(outcome.write, commentActionWrite);
      const mine = writes.idOf(key);
      if (action === undefined || mine === undefined || action.id >= mine) return;
      if (outcome.kind === "refused") this.#refused = true;
      this.#left?.();
    });
  }

  /** Waits for every action pressed before this one to land, and answers this action's own row
   *  in the store, which joined before its turn came. */
  wait(signal: AbortSignal | undefined): Promise<number> {
    const writes = this.#writes;
    if (signal?.aborted === true) return Promise.reject(new CommentActionStoppedError());
    const mine = writes.idOf(this.#key);
    if (mine === undefined || this.#refused) return Promise.reject(new CommentActionClearedError());
    const earlier = (): boolean =>
      writes.rows().some((row) => {
        const action = ofKind(row, commentActionWrite);
        return action !== undefined && action.id < mine && liveWrite(action);
      });
    if (!earlier()) return Promise.resolve(mine);
    const waited = Promise.withResolvers<number>();
    const stopped = (): void => waited.reject(new CommentActionStoppedError());
    signal?.addEventListener("abort", stopped, { once: true });
    this.#left = () => {
      if (this.#refused) waited.reject(new CommentActionClearedError());
      else if (!earlier()) waited.resolve(mine);
    };
    return waited.promise.finally(() => {
      this.#left = undefined;
      signal?.removeEventListener("abort", stopped);
    });
  }
}

/**
 * Every comment state change the page makes, everywhere comments render: each press is one write
 * of the store on its comment's line (`comment:<id>`), kept with the action it will make, so it
 * reaches Dispatch whatever becomes of the card - another page, an unmount, a reload.
 *
 * Two behaviours of the comment list hold, read from the store's rows rather than from a map of
 * the component's (`EarlierActions`): an action on another comment waits for the one in flight,
 * and a failure clears the actions still waiting - those held in their line by a sign-out or a
 * lapse included - which show nothing and are never sent. A second press of an action already
 * in line is refused rather than queued - the card offered one press, and the store holds it -
 * and the card that offered a refused action shows its refusal with a Retry
 * (`CommentActionFailure`).
 */
export function useCommentActionQueue({ onMutate, onSuccess, shows }: CommentActionQueueOptions) {
  const writes = useWrites();
  const [refused, setRefused] = useState<Refused | undefined>(undefined);
  const inLine = useWriteSelection("comment-actions", (rows) =>
    rows.flatMap((row) => {
      const action = ofKind(row, commentActionWrite);
      return action !== undefined && liveWrite(action) ? [action.detail] : [];
    })
  );
  const pendingActionIds = useMemo(() => new Set(inLine.map((action) => action.id)), [inLine]);

  function press(input: CommentActionInput, key: string): void {
    const request = commentActionRequest(input);
    let begun = false;
    setRefused(undefined);
    // Watched from the press, not from each attempt: a refusal ahead of this action that came
    // while it was held is still read by the attempt after the hold.
    const earlier = new EarlierActions(writes, key);
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
          // Once begun, the action has gone out and every action before it had landed: none can
          // be refused ahead of it, and an attempt after a hold is the same request again.
          if (!begun) {
            const mine = await earlier.wait(bounds.signal);
            // What the card shows while the action is out is written as it goes out, so an
            // action cleared behind a failure never showed anything to put back. From there the
            // change is the store's: it shows it again over each answer that lands on a cache it
            // wrote until this action settles (R7 of dispatch://INFRA-299), and puts it back if
            // the action leaves without landing (R10). An action the store let go or held while
            // `onMutate` ran - either aborts this attempt - is sent no more than one let go or
            // held while it waited.
            await onMutate?.(input);
            if (bounds.signal?.aborted === true) throw new CommentActionStoppedError();
            const change = shows?.(input);
            if (change !== undefined) writes.shows(mine, change);
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
          setRefused({
            failure: actionFailure(input.id, refusal),
            input,
            key: retriesUnderItsKey(refusal) ? key : undefined,
          });
        }
      )
      .finally(earlier.stop);
  }

  return {
    actionFailure: refused?.failure,
    mutateItem: (input: CommentActionInput) => {
      // A second press of an action already in line is the same action: the store holds it.
      if (inLine.some((action) => action.id === input.id && action.kind === input.kind)) return;
      press(input, newSendKey());
    },
    pendingActionIds,
    retryItem: () => {
      if (refused !== undefined) press(refused.input, refused.key ?? newSendKey());
    },
  };
}
