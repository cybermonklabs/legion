import {
  type DefaultError,
  type UseMutationOptions,
  type UseMutationResult,
  useMutation,
} from "@tanstack/react-query";
import { useCallback, useRef, useState } from "react";

import type { WriteBounds, WriteRequest } from "../../api/client";
import { pressKey, type StoppedPress, stoppedPress } from "./keyed-write";
import { plainWrite, useWrites, type WriteChange, type WriteLine } from "./write-store";

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
  /** Why this write's answer carries something shown once and never again, for a write of that
   *  kind, which a reload drops rather than sends again (`WriteSpec.answerShownOnce`). */
  readonly answerShownOnce?: string;
  /** The change this press shows, for a write that shows one: the store shows it as the write
   *  joins - after `onMutate`, which is where the surface snapshots what a refusal rolls back to
   *  - and again over each answer that lands on a cache it wrote while the write is live (R7 of
   *  dispatch://INFRA-299, `WriteSpec.shows`). A write that shows nothing names none. */
  readonly shows?: (variables: TVariables) => WriteChange;
  /** Whether the store puts that change back as the write leaves its line without landing, for
   *  a surface whose `onError` rolls nothing back itself (`WriteSpec.storePutsBack`). Left out,
   *  the surface rolls its own change back. */
  readonly storePutsBack?: boolean;
} & KeyedWriteMade<TData, TVariables>;

/** How a write is made, and the line it is ordered on: by an attempt of its own (`write`, under
 *  the bounds the store gives it: its key, its login and its deadline), which then says why it is
 *  on no line (`unlined`), or, on a line, from its request alone. Writes to one target - one
 *  issue's fields - go out in press order, so an older one never lands after a newer one; each
 *  names the request its client method's builder makes (`issuePatchRequest`), which is what goes
 *  out (`api.send`) and what a write waiting its turn is kept as for a reload, so the two can
 *  never differ, and nothing of it runs until its turn comes (`WriteSpec.request`). One of
 *  `target` and `unlined` is always named, so a write written later cannot leave its line out in
 *  silence. */
export type KeyedWriteMade<TData, TVariables> =
  | (Extract<WriteLine, { readonly unlined: string }> & {
      readonly write: (variables: TVariables, bounds: WriteBounds) => Promise<TData>;
      readonly request?: undefined;
    })
  | (Omit<Extract<WriteLine, { readonly target: string }>, "target"> & {
      readonly write?: undefined;
      /** The row each press writes (`WriteLine`'s `target`), named from its variables. */
      readonly target: (variables: TVariables) => string;
      readonly request: (variables: TVariables) => WriteRequest<TData>;
    });

/** A press Dispatch made and had no answer to give for: its variables, and when it landed. */
export interface UnansweredLanding<TVariables> {
  readonly variables: TVariables;
  readonly at: number;
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
};

/**
 * The one way a surface that is not a composer writes to Dispatch: each `mutate` is one press,
 * made under a key of its own and tried under it until Dispatch decides it, shown by the shell's
 * pending-writes indicator while it is out and, refused, with Retry and Discard (`Writes.run`).
 * The mutation settles with Dispatch's answer - `null` for a landing without one - its refusal, or
 * `SendStoppedError` once the reader stops it, so the surface keeps its optimistic state until
 * then and rolls it back on anything but a landing. A `mutate` of the same variables a press the
 * reader stopped was made with is that press made again, under its key - Dispatch may have taken
 * it, and answers the key with what it did - while one after Dispatch's own refusal is a new
 * press, since Dispatch would answer the old key with that refusal. It runs whether or not the
 * browser says it is online (`networkMode: "always"`): the write's own tries wait out a network
 * that is down, and a mutation TanStack paused offline would make none, never go late and never
 * offer Stop trying. A press that lands with no answer is kept as `unanswered` for the surface
 * to say so, until the next press or `forgetUnanswered`.
 */
export function useKeyedWrite<TData, TVariables = void, TContext = unknown>({
  answerShownOnce,
  endsWithSession,
  file,
  inline,
  label,
  request,
  shows,
  storePutsBack,
  target,
  unlined,
  write,
  ...options
}: KeyedWriteOptions<TData, TVariables, TContext>): KeyedWriteResult<TData, TVariables, TContext> {
  const writes = useWrites();
  /** The last press the reader stopped, which a Retry of the same variables makes again. */
  const stopped = useRef<StoppedPress<TVariables> | undefined>(undefined);
  const [unanswered, setUnanswered] = useState<UnansweredLanding<TVariables> | undefined>(
    undefined
  );
  const forgetUnanswered = useCallback(() => setUnanswered(undefined), []);
  const mutation = useMutation({
    ...options,
    mutationFn: (variables: TVariables) => {
      const again = stopped.current;
      stopped.current = undefined;
      setUnanswered(undefined);
      const key = pressKey(again, variables);
      const press = {
        answerShownOnce,
        detail: null,
        endsWithSession,
        file: file?.(variables),
        inline: typeof inline === "function" ? inline(variables) : inline,
        key,
        kind: plainWrite,
        label: label(variables),
        presentation: "indicator",
        shows: shows === undefined ? undefined : shows(variables),
        storePutsBack,
      } as const;
      const line =
        write === undefined
          ? { request: request(variables), target: target(variables) }
          : { unlined, write: (bounds: WriteBounds) => write(variables, bounds) };
      return writes.run({ ...press, ...(line satisfies WriteLine) }).then(
        (answer) => {
          if (answer === null) setUnanswered({ at: Date.now(), variables });
          return answer;
        },
        (error: unknown) => {
          stopped.current = stoppedPress(key, variables, error) ?? stopped.current;
          throw error;
        }
      );
    },
    networkMode: "always",
  });
  return { ...mutation, forgetUnanswered, unanswered };
}
