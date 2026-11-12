import {
  type DefaultError,
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

/** A press the reader stopped: the key its attempts went out under, and the variables it was
 *  made with, which are that press's own handle - `mutate` of this very object is the press made
 *  again, under this key (`Object.is`), and nothing else reaches it. */
interface StoppedPress<TVariables> {
  readonly key: string;
  readonly variables: TVariables;
}

/** `useMutation`'s result, with the one "made, but Dispatch lost its answer" state every create
 *  form shows (`LandedWithoutAnswer`): the last press that landed with `null`, until the next
 *  press starts or the form lets it go (`forgetUnanswered`, on an edit). */
export type KeyedWriteResult<TData, TVariables, TContext> = UseMutationResult<
  TData | null,
  DefaultError,
  TVariables,
  TContext
> & {
  readonly unanswered: UnansweredLanding<TVariables> | undefined;
  readonly forgetUnanswered: () => void;
  /** The press the reader stopped that `wanted` picks out, or `undefined` where no stopped press
   *  of this hook is that one. The hook holds every press that ended stopped, newest first, each
   *  under the key its attempts went out under, and the variables object it hands back is that
   *  press's handle: a `mutate` of that very object is the press made again under its key, and
   *  nothing else reaches it. More than one press of a hook can be out at once - a paste of
   *  several files is several presses - so the surface says which press it means: the refusal it
   *  shows follows its newest press, which is not always the press that ended stopped last. So a
   *  surface takes the press its refusal is about, under `retriesUnderItsKey` of that refusal,
   *  and sends it back as it stands rather than building variables of its own, which would be a
   *  press of its own. */
  readonly stopped: (wanted: (variables: TVariables) => boolean) => TVariables | undefined;
};

/**
 * The one way a surface that is not a composer writes to Dispatch: each `mutate` is one press,
 * made under a key of its own and tried under it until Dispatch decides it, shown by the shell's
 * pending-writes indicator while it is out and, refused, with Retry and Discard (`Writes.run`).
 * The mutation settles with Dispatch's answer - `null` for a landing without one - its refusal, or
 * `SendStoppedError` once the reader stops it, so the surface keeps its optimistic state until
 * then and rolls it back on anything but a landing. A `mutate` of the very variables object a
 * press the reader stopped was made with - the object `stopped` hands back - is that press made
 * again, under its key: Dispatch may have taken it, and answers the key with what it did. A
 * press after Dispatch's own refusal is a press of its own, since Dispatch would answer the old
 * key with that refusal. Every stopped press is held, each under its own key, and a surface
 * reaches one only by passing that press back (`stopped`), so a key belongs to the press that
 * made it rather than to the values that press carried. A press made again from a form, with
 * equal values but a new object, is therefore a new press under a new key, and may write a
 * second time where the first had landed; on the upload surfaces here the document token
 * answers that, since the second upload names the base its page read, and Dispatch refuses it
 * `409 PRECONDITION_FAILED` with the token and version the document is at now, which the row
 * says - so the person is told rather than their press being dropped in silence. It
 * runs whether or not the browser says it is online (`networkMode: "always"`): the write's own
 * tries wait out a network that is down, and a mutation TanStack paused offline would make none,
 * never go late and never offer Stop trying. A press that lands with no answer is kept as
 * `unanswered` for the surface to say so, until the next press or `forgetUnanswered`.
 */
export function useKeyedWrite<TData, TVariables = void, TContext = unknown>({
  endsWithSession,
  file,
  inline,
  label,
  request,
  target,
  write,
  ...options
}: KeyedWriteOptions<TData, TVariables, TContext>): KeyedWriteResult<TData, TVariables, TContext> {
  const writes = useWrites();
  /** Every press the reader stopped and has not made again, newest first. A surface with more
   *  than one press out at once - a paste of several files - asks for the one it means. */
  const stopped = useRef<readonly StoppedPress<TVariables>[]>([]);
  const [unanswered, setUnanswered] = useState<UnansweredLanding<TVariables> | undefined>(
    undefined
  );
  const forgetUnanswered = useCallback(() => setUnanswered(undefined), []);
  const stoppedPress = useCallback(
    (wanted: (variables: TVariables) => boolean) =>
      stopped.current.find((press) => wanted(press.variables))?.variables,
    []
  );
  const mutation = useMutation({
    ...options,
    mutationFn: (variables: TVariables) => {
      // Only the very object the stopped press was made with reaches it (`stopped`): a press
      // built afresh is a press of its own, whatever its values read like.
      const again = stopped.current.find((press) => Object.is(press.variables, variables));
      if (again !== undefined) {
        stopped.current = stopped.current.filter((press) => press !== again);
      }
      setUnanswered(undefined);
      const key = again?.key ?? newSendKey();
      const press = {
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
          ? writes.run({ ...press, request: request(variables), target: target(variables) })
          : writes.run({ ...press, write: (bounds) => write(variables, bounds) })
      ).then(
        (answer) => {
          if (answer === null) setUnanswered({ at: Date.now(), variables });
          return answer;
        },
        (error: unknown) => {
          if (retriesUnderItsKey(error instanceof Error ? error : undefined)) {
            stopped.current = [{ key, variables }, ...stopped.current];
          }
          throw error;
        }
      );
    },
    networkMode: "always",
  });
  return { ...mutation, forgetUnanswered, stopped: stoppedPress, unanswered };
}
