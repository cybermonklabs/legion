import {
  type DefaultError,
  type MutateOptions,
  type UseMutateAsyncFunction,
  type UseMutateFunction,
  type UseMutationOptions,
  type UseMutationResult,
  useMutation,
} from "@tanstack/react-query";
import { useCallback, useRef, useState } from "react";

import type { WriteBounds, WriteRequest } from "../../api/client";
import { newSendKey } from "../../lib/send-key";
import { retriesUnderItsKey } from "./keyed-write";
import { plainWrite, useWrites } from "./write-store";

/** A write a surface makes for the reader: `useMutation`'s options, but for `mutationFn`, which
 *  the write path makes from `write` or `request` and `label`. Its data is Dispatch's answer, or
 *  `null` for a write Dispatch made and has no answer to give for (`409 KEY_COMMITTED`, or a
 *  replay of an answer it did not keep): a landing all the same, which `onSuccess` handles - a
 *  create form lets go of its draft and says what was made (`LandedWithoutAnswer`) rather than
 *  opening it. */
export type KeyedWriteOptions<TData, TVariables, TContext> = Omit<
  UseMutationOptions<TData | null, DefaultError, TVariables, TContext>,
  "mutationFn" | "networkMode"
> & {
  /** What the shell's pending-writes indicator names this write, in the reader's words. */
  readonly label: (variables: TVariables) => string;
  /** Whether this surface shows a refusal itself (a form the reader is in front of): the
   *  indicator then shows the write only while it is out. A function decides it per write. */
  readonly inline?: boolean | ((variables: TVariables) => boolean);
  /** An upload's file, kept across a reload with the write. */
  readonly file?: (variables: TVariables) => File | undefined;
  /** Whether the write ends the person's session (a sign-out): a lapse of that session lands it
   *  rather than holding it for the next sign-in (`WriteSpec.endsWithSession`). */
  readonly endsWithSession?: boolean;
} & KeyedWriteMade<TData, TVariables>;

/** How a write is made: by an attempt of its own (`write`, under the bounds the store gives it:
 *  its key, its login and its deadline), or, on a line, from its request alone. Writes to one
 *  target - one issue's fields - go out in press order, so an older one never lands after a newer
 *  one; each names the request its client method's builder makes (`issuePatchRequest`), which is
 *  what goes out (`api.send`) and what a write waiting its turn is kept as for a reload, so the
 *  two can never differ, and nothing of it runs until its turn comes (`WriteSpec.request`). */
export type KeyedWriteMade<TData, TVariables> =
  | {
      readonly write: (variables: TVariables, bounds: WriteBounds) => Promise<TData>;
      readonly target?: undefined;
      readonly request?: undefined;
    }
  | {
      readonly write?: undefined;
      readonly target: (variables: TVariables) => string | undefined;
      readonly request: (variables: TVariables) => WriteRequest<TData>;
    };

/** A press Dispatch made and had no answer to give for: its variables, and when it landed. */
export interface UnansweredLanding<TVariables> {
  readonly variables: TVariables;
  readonly at: number;
}

/** One press of a keyed write: the variables it was made with and, out of a surface's reach, the
 *  key its attempts go out under, minted when the press was made. TanStack runs the write over
 *  these, so each key travels with its own press however many presses are out at once, and a
 *  surface that reads a keyed write in TanStack's mutation cache reads its presses' `variables`
 *  (`useDefaultAssigneeWrite`). A press the reader stopped is handed back by `stopped` as this very
 *  object, which is its handle: the hook finds what it holds for a press by that object alone,
 *  never by the variables it carries. */
export interface KeyedPress<TVariables> {
  readonly variables: TVariables;
}

/** A press as the hook makes it and holds it. */
interface Press<TVariables> extends KeyedPress<TVariables> {
  readonly key: string;
}

/** `useMutation`'s result, with the one "made, but Dispatch lost its answer" state every create
 *  form shows (`LandedWithoutAnswer`): the last press that landed with `null`, until the next
 *  press starts or the form lets it go (`forgetUnanswered`, on an edit); and the Retry of a press
 *  the reader stopped, which only the surface's own Retry makes (`retry`, `stopped`, `again`). */
export type KeyedWriteResult<TData, TVariables, TContext> = UseMutationResult<
  TData | null,
  DefaultError,
  TVariables,
  TContext
> & {
  readonly unanswered: UnansweredLanding<TVariables> | undefined;
  readonly forgetUnanswered: () => void;
  /** The surface's Retry of its newest press, the press its refusal is about. Where the reader
   *  stopped that press it is made again, under its key (`again`): it may have reached Dispatch,
   *  which answers the key with what it did there. Where Dispatch refused it, the Retry is a new
   *  press of the same variables under a new key, since Dispatch would answer the old key with
   *  that refusal. Nothing where the hook has made no press. */
  readonly retry: () => void;
  /** The press the reader stopped that `wanted` picks out by its variables, or `undefined` where
   *  the hook holds no stopped press that is that one. The hook holds every press that ended
   *  stopped and has not been made again, newest first, each under the key its attempts went out
   *  under. More than one press of a hook can be out at once - a paste of several files is several
   *  presses - and the refusal a surface shows follows its newest press, which is not always the
   *  press the reader stopped last. So such a surface's Retry names the press its refusal is
   *  about, under `retriesUnderItsKey` of that refusal, and makes that press again (`again`). */
  readonly stopped: (
    wanted: (variables: TVariables) => boolean
  ) => KeyedPress<TVariables> | undefined;
  /** Makes `press`, which `stopped` handed back, again under its own key. It leaves the presses
   *  the hook holds while it is out, and is held again, under the same key, if the reader stops
   *  it again. A press the hook does not hold - out again already, or decided - is not made. */
  readonly again: (press: KeyedPress<TVariables>) => void;
};

/** The callbacks a surface hands one `mutate`, as TanStack calls them with the press the hook made
 *  of it: each is handed that press's variables, as it would be from a `useMutation` of its own. */
function pressCallbacks<TData, TVariables, TContext>(
  callbacks: MutateOptions<TData, DefaultError, TVariables, TContext> | undefined
): MutateOptions<TData, DefaultError, Press<TVariables>, TContext> | undefined {
  if (callbacks === undefined) {
    return undefined;
  }
  const { onError, onSettled, onSuccess } = callbacks;
  return {
    onError:
      onError &&
      ((error, press, result, context) => onError(error, press.variables, result, context)),
    onSettled:
      onSettled &&
      ((data, error, press, result, context) =>
        onSettled(data, error, press.variables, result, context)),
    onSuccess:
      onSuccess &&
      ((data, press, result, context) => onSuccess(data, press.variables, result, context)),
  };
}

/**
 * The one way a surface that is not a composer writes to Dispatch: each `mutate` is one press,
 * made under a key of its own and tried under it until Dispatch decides it, shown by the shell's
 * pending-writes indicator while it is out and, refused, with Retry and Discard (`Writes.run`).
 * The mutation settles with Dispatch's answer - `null` for a landing without one - its refusal, or
 * `SendStoppedError` once the reader stops it, so the surface keeps its optimistic state until
 * then and rolls it back on anything but a landing.
 *
 * A press the reader stopped is no verdict: it may have reached Dispatch, which answers its key
 * with what it did there. So the hook holds every press that ended stopped, under its key, and
 * that press belongs to the press that made it: only the surface's own Retry reaches it, through
 * `retry` where the Retry is of the surface's newest press, or through the handle `stopped` gives
 * and `again` where more than one press is out at once. A `mutate` or `mutateAsync` is a press
 * the person made anew and always goes under a new key, whatever its variables are: the hook
 * never matches a press by its variables, by identity or by value, so a later press of equal
 * values - a title typed again, Approve after a change request - is a write Dispatch decides,
 * never one it answers with what the stopped press did there while the person's own choice is
 * dropped in silence. Where the stopped press had landed, the later press is a second write:
 * Dispatch refuses it where that would be wrong - a name the owner holds now, an upload's base
 * the document has moved past - and the person reads that refusal, and a field ends at the value
 * the person chose last. A Retry after Dispatch's own refusal is a press of its own too, since
 * Dispatch would answer the old key with that refusal.
 *
 * TanStack runs each press as the hook made it (`KeyedPress`), the key with it, so the key goes
 * with its own press however many are out at once; the surface's callbacks and the result's
 * `variables` are the press's own variables, as from a `useMutation` of its own. It runs whether
 * or not the browser says it is online (`networkMode: "always"`): the write's own tries wait out a
 * network that is down, and a mutation TanStack paused offline would make none, never go late and
 * never offer Stop trying. A press that lands with no answer is kept as `unanswered` for the
 * surface to say so, until the next press or `forgetUnanswered`.
 */
export function useKeyedWrite<TData, TVariables = void, TContext = unknown>({
  endsWithSession,
  file,
  inline,
  label,
  onError,
  onMutate,
  onSettled,
  onSuccess,
  request,
  target,
  write,
  ...options
}: KeyedWriteOptions<TData, TVariables, TContext>): KeyedWriteResult<TData, TVariables, TContext> {
  const writes = useWrites();
  /** Every press the reader stopped and has not made again, newest first. Each is reached by its
   *  own object alone: `again` of the handle `stopped` gave, or `retry` of the newest press. */
  const held = useRef<readonly Press<TVariables>[]>([]);
  const [unanswered, setUnanswered] = useState<UnansweredLanding<TVariables> | undefined>(
    undefined
  );
  const forgetUnanswered = useCallback(() => setUnanswered(undefined), []);
  const mutation = useMutation<TData | null, DefaultError, Press<TVariables>, TContext>({
    ...options,
    mutationFn: (press) => {
      const { key, variables } = press;
      setUnanswered(undefined);
      const spec = {
        detail: null,
        endsWithSession,
        file: file?.(variables),
        inline: typeof inline === "function" ? inline(variables) : inline,
        key,
        kind: plainWrite,
        label: label(variables),
        presentation: "indicator",
      } as const;
      return (
        write === undefined
          ? writes.run({ ...spec, request: request(variables), target: target(variables) })
          : writes.run({ ...spec, write: (bounds) => write(variables, bounds) })
      ).then(
        (answer) => {
          if (answer === null) setUnanswered({ at: Date.now(), variables });
          return answer;
        },
        (error: unknown) => {
          if (retriesUnderItsKey(error instanceof Error ? error : undefined)) {
            held.current = [press, ...held.current];
          }
          throw error;
        }
      );
    },
    networkMode: "always",
    onError:
      onError &&
      ((error, press, result, context) => onError(error, press.variables, result, context)),
    onMutate: onMutate && ((press, context) => onMutate(press.variables, context)),
    onSettled:
      onSettled &&
      ((data, error, press, result, context) =>
        onSettled(data, error, press.variables, result, context)),
    onSuccess:
      onSuccess &&
      ((data, press, result, context) => onSuccess(data, press.variables, result, context)),
  });
  const { mutate: makePress, mutateAsync: makePressAsync } = mutation;
  const mutate = useCallback(
    (
      ...[variables, callbacks]: Parameters<
        UseMutateFunction<TData | null, DefaultError, TVariables, TContext>
      >
    ) =>
      makePress(
        { key: newSendKey(), variables: variables as TVariables },
        pressCallbacks(callbacks)
      ),
    [makePress]
  );
  const mutateAsync = useCallback(
    (
      ...[variables, callbacks]: Parameters<
        UseMutateAsyncFunction<TData | null, DefaultError, TVariables, TContext>
      >
    ) =>
      makePressAsync(
        { key: newSendKey(), variables: variables as TVariables },
        pressCallbacks(callbacks)
      ),
    [makePressAsync]
  );
  const stopped = useCallback(
    (wanted: (variables: TVariables) => boolean) =>
      held.current.find((press) => wanted(press.variables)),
    []
  );
  const again = useCallback(
    (press: KeyedPress<TVariables>) => {
      const made = held.current.find((each) => each === press);
      if (made === undefined) {
        return;
      }
      held.current = held.current.filter((each) => each !== made);
      makePress(made);
    },
    [makePress]
  );
  const newest = mutation.variables;
  const retry = useCallback(() => {
    if (newest === undefined) {
      return;
    }
    if (held.current.includes(newest)) {
      again(newest);
      return;
    }
    mutate(newest.variables);
  }, [again, mutate, newest]);
  // TanStack's result is a union over `status`, each member with the press it ran, and the
  // press's own variables stand in for it member by member, which the spread does not show.
  return {
    ...mutation,
    again,
    forgetUnanswered,
    mutate,
    mutateAsync,
    retry,
    stopped,
    unanswered,
    variables: newest?.variables,
  } as KeyedWriteResult<TData, TVariables, TContext>;
}
