import {
  type DefaultError,
  type MutateOptions,
  type MutationFunctionContext,
  type MutationKey,
  type UseMutateAsyncFunction,
  type UseMutateFunction,
  type UseMutationOptions,
  type UseMutationResult,
  useMutation,
  useMutationState,
} from "@tanstack/react-query";
import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";

import {
  apiErrorMessage,
  type WriteBounds,
  type WriteMethod,
  type WriteRequest,
} from "../../api/client";
import { type EndReadHold, holdBackReads, type ReadHold } from "../../api/query-client";
import { newSendKey } from "../../lib/send-key";
import {
  refusedByDispatch,
  retriesUnderItsKey,
  SendStoppedError,
  stoppedWriteNote,
} from "./keyed-write";
import { plainWrite, useWrites, type Write, type WriteKind, type Writes } from "./write-store";

/** A write a surface makes for the reader: `useMutation`'s options, but for `mutationFn`, which
 *  the write path makes from `write` or `request` and `label`. Its data is Dispatch's answer, or
 *  `null` for a write Dispatch made and has no answer to give for (`409 KEY_COMMITTED`, or a
 *  replay of an answer it did not keep): a landing all the same, which `onSuccess` handles - a
 *  create form lets go of its draft and says what was made (`LandedWithoutAnswer`) rather than
 *  opening it. */
export type KeyedWriteOptions<TData, TVariables, TContext, D = null, K = D> = Omit<
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
  /** The queries this write's optimistic value stands in, whose reads that start from the press
   *  wait while it is out (`holdBackReads`): until its answer or its refusal is in, its own live
   *  event says Dispatch took it (`ReadHold.endsOn`), or `SEND_DEADLINE_MS` has passed. The press's
   *  `onMutate` writes that value, and its `onSuccess` or `onError` puts the answer or the
   *  rollback in its place before the hold ends. A write of queries other people write names its
   *  event, or holds nothing. */
  readonly holds?: (variables: TVariables) => ReadHold;
  /** `false` for a write that goes under no key whatever its method (`WriteSpec.keyed`). */
  readonly keyed?: false;
  /** Whether the surface reads the result's `late`: only such a hook follows the store for it, so
   *  a hook whose surface does not adds no listener to the store. */
  readonly readsLate?: true;
} & KeyedWriteMade<TData, TVariables> &
  KeyedWriteKind<TVariables, D, K>;

/** The kind of write each press is (`WriteKind`), with its record made from the press's
 *  variables: what the store keeps of it beyond its request, and what its kind reads - the
 *  reader's own text, say, which a surface keeps when Dispatch refuses it. Left out, a plain
 *  write with no record (`plainWrite`). */
export type KeyedWriteKind<TVariables, D, K> =
  | { readonly kind?: undefined; readonly detail?: undefined }
  | { readonly kind: WriteKind<D, K>; readonly detail: (variables: TVariables) => D };

/** How a write is made: by an attempt of its own (`write`, under the bounds the store gives it:
 *  its key, its login and its deadline), with the `method` of the request it makes, which the
 *  write names because no attempt says it until it goes out (`WriteSpec.method`); or, on a line,
 *  from its request alone. Writes to one target - one issue's fields - go out in press order, so
 *  an older one never lands after a newer one; each names the request its client method's
 *  builder makes (`issuePatchRequest`), which is what goes out (`api.send`) and what a write
 *  waiting its turn is kept as for a reload, so the two can never differ, and nothing of it runs
 *  until its turn comes (`WriteSpec.request`). */
export type KeyedWriteMade<TData, TVariables> =
  | {
      readonly write: (variables: TVariables, bounds: WriteBounds) => Promise<TData>;
      /** The method of the request `write` makes, or, where a press's variables decide it, the
       *  method of that press's. */
      readonly method: WriteMethod | ((variables: TVariables) => WriteMethod);
      readonly target?: undefined;
      readonly request?: undefined;
    }
  | {
      readonly write?: undefined;
      readonly method?: undefined;
      readonly target: (variables: TVariables) => string | undefined;
      readonly request: (variables: TVariables) => WriteRequest<TData>;
    };

/** A press Dispatch made and had no answer to give for: its variables, and when it landed. */
export interface UnansweredLanding<TVariables> {
  readonly variables: TVariables;
  readonly at: number;
}

/** The end of each press's hold on the reads it writes over (`KeyedWriteOptions.holds`), by the
 *  context TanStack makes for that press's run and hands to each of its callbacks. */
const pressHolds = new WeakMap<MutationFunctionContext, EndReadHold>();

/** Ends the hold of the press TanStack runs under `context`, if it keeps one still: `refused` is
 *  whether Dispatch refused the press with no earlier attempt of it that may have landed unseen,
 *  in any try of the press (`refusedByDispatch`), which then publishes no event for the hold to
 *  wait for. */
function endHold(context: MutationFunctionContext, refused = false): void {
  pressHolds.get(context)?.(refused ? 1 : 0);
  pressHolds.delete(context);
}

/** One press of a keyed write: the variables it was made with and, out of a surface's reach, the
 *  key its attempts go out under, minted when the press was made. TanStack runs the write over
 *  these, so each key travels with its own press however many presses are out at once, and a
 *  surface that reads a keyed write in TanStack's mutation cache reads its presses' `variables`
 *  (`useProjectSettingWrite`). A press the reader stopped is handed back by `stopped` as this very
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
 *  the reader stopped, which the surface makes again (`retry`, `stopped`, `again`). The shell's
 *  pending writes can make a stopped press again too; once that lands the hook lets the press go
 *  and, where it is the press the hook made last, clears its error, as `useKeyedWrite` says. */
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
   *  which answers a `POST` or a `PATCH` under that key with what it did there, and a `PUT` or a
   *  `DELETE`, which sends no key, is the same change made again. Where Dispatch refused it, the
   *  Retry is a new press of the same variables under a new key, since Dispatch would answer the
   *  old key with that refusal. Nothing where the hook has made no press. */
  readonly retry: () => void;
  /** The press the reader stopped that `wanted` picks out by its variables, or `undefined` where
   *  the hook holds no stopped press that is that one. The hook holds every press that ended
   *  stopped and has not been made again or landed by the shell's Retry, newest first, each under
   *  the key its attempts went out under. More than one press of a hook can be out at once - a
   *  paste of several files is several presses - and the refusal a surface shows follows its
   *  newest press, which is not always the press the reader stopped last. So such a surface's
   *  Retry names the press its refusal is about, under `retriesUnderItsKey` of that refusal, and
   *  makes that press again (`again`). */
  readonly stopped: (
    wanted: (variables: TVariables) => boolean
  ) => KeyedPress<TVariables> | undefined;
  /** Makes `press`, which `stopped` handed back, again under its own key, and says whether it
   *  did. It leaves the presses the hook holds while it is out, and is held again, under the same
   *  key, if the reader stops it again. A press the hook does not hold - out again already, or
   *  decided - is not made, and `again` returns `false`. */
  readonly again: (press: KeyedPress<TVariables>) => boolean;
  /** The method of the newest press's request where the reader stopped that press, the press
   *  `retry` makes again, so a surface's sentence of that Retry reads it here rather than stating
   *  it a second time (`endedStopped`, `stoppedWriteNote`); `undefined` while the newest press did
   *  not end stopped. */
  readonly method: WriteMethod | undefined;
  /** Whether the newest press is past a deadline: its write is out past its own
   *  (`SEND_DEADLINE_MS`), or waits its turn behind a write to its target that is. Either way it
   *  is still pending, tried under its key - or, waiting, to be - until Dispatch decides it or the
   *  reader stops it at the shell's indicator. A surface that holds its controls while its write
   *  is out lets them go here, so a write that never answers - offline, its tries waiting out the
   *  network - locks none of them, and a press made then, waiting behind it, locks none again.
   *  Read only where the write was made with `readsLate`; `false` for any other. */
  readonly late: boolean;
  /** Whether the press made with `variables` - those very values, by identity - is out past a
   *  deadline, as `late` is of the newest press: for a surface that holds more than one press of
   *  the write at once, each by what it is of (an Inbox's write of every ask, a draft's uploads).
   *  `false` for a press that is not out. Read only where the write was made with `readsLate`;
   *  `false` for any other. */
  readonly lateOf: (variables: TVariables) => boolean;
};

/** Whether the newest press of `write` ended with the reader's stop: its `error` is then that
 *  `SendStoppedError`, and its `method` the method of that press's request. */
export function endedStopped<
  W extends { readonly error: unknown; readonly method: WriteMethod | undefined },
>(write: W): write is W & { readonly error: SendStoppedError; readonly method: WriteMethod } {
  return write.error instanceof SendStoppedError && write.method !== undefined;
}

/** Whether the write the store holds under `key` is late, or waits its turn on its target's line
 *  behind a write that is (`KeyedWriteResult.late`). */
function lateIn(rows: readonly Write[], key: string): boolean {
  const own = rows.find((row) => row.key === key);
  if (own === undefined) return false;
  if (own.status === "late") return true;
  return (
    own.status === "queued" &&
    own.target !== undefined &&
    rows.some(
      (row) => row.target === own.target && row.joined < own.joined && row.status === "late"
    )
  );
}

/** Whether the write the store holds under `key` is late (`lateIn`), followed only while there is
 *  a `key`: `KeyedWriteResult.late`, and the lateness of a press `usePendingKeyedPress` finds. */
function useLateIn(writes: Writes, key: string | undefined): boolean {
  const follow = useCallback(
    (listener: () => void) => (key === undefined ? () => undefined : writes.subscribe(listener)),
    [key, writes]
  );
  return useSyncExternalStore(follow, () => key !== undefined && lateIn(writes.rows(), key));
}

/** The keys among `keys()` whose write the store holds late (`lateIn`), followed only while
 *  `following`, as one string of them, so a change of the store that turns none of them late
 *  draws nothing again. */
function useLateKeys(writes: Writes, following: boolean, keys: () => readonly string[]): string {
  const subscribe = useCallback(
    (listener: () => void) => (following ? writes.subscribe(listener) : () => undefined),
    [following, writes]
  );
  return useSyncExternalStore(subscribe, () => {
    if (!following) return "";
    const rows = writes.rows();
    return keys()
      .filter((key) => lateIn(rows, key))
      .join(" ");
  });
}

/** The keys among `keys` whose write the store holds late, or waiting its turn on its target's
 *  line behind a write that is (`lateIn`), as `KeyedWriteResult.late` reads a keyed write's. For
 *  a surface that makes its writes with `Writes.run` under keys of its own rather than through
 *  this hook, and holds a control while it waits on them: it lets the control go once each write
 *  it waits on is late, so a write that never answers - offline, its tries waiting out the
 *  network - locks nothing. A key the store holds no write under - not joined yet, or decided -
 *  is not late. The set is the same object while the same keys are late, and the store is
 *  followed only while `keys` names any. */
export function useLateWrites(keys: readonly string[]): ReadonlySet<string> {
  const late = useLateKeys(useWrites(), keys.length > 0, () => keys);
  return useMemo(() => new Set(late === "" ? [] : late.split(" ")), [late]);
}

/** The pending press of a keyed write shared by exact mutation key. A surface that mounts
 *  while the press is out can release its hold at that same press's deadline. `of`, where given,
 *  picks out the presses of what the surface is drawn for, by their variables: a hook whose one
 *  key serves each subject in turn - a card drawn for one ask after another - keeps its key, and
 *  with it the press it holds, as the surface moves on. */
export function usePendingKeyedPress<TVariables = unknown>(
  mutationKey: MutationKey,
  of?: (variables: TVariables) => boolean
): {
  readonly isPending: boolean;
  readonly late: boolean;
  readonly submittedAt: number;
} {
  const writes = useWrites();
  const press = useMutationState({
    filters: {
      exact: true,
      mutationKey,
      predicate:
        of === undefined
          ? undefined
          : ({ state }) => of((state.variables as Press<TVariables>).variables),
      status: "pending",
    },
    select: ({ state }) => ({
      key: (state.variables as Press<unknown> | undefined)?.key,
      submittedAt: state.submittedAt,
    }),
  }).at(-1);
  const late = useLateIn(writes, press?.key);
  return { isPending: press !== undefined, late, submittedAt: press?.submittedAt ?? 0 };
}

/** The sentence a surface shows for its write where the reader stopped it, under the plain Retry
 *  the surface offers that makes that very press again (`stoppedWriteNote`, `retry`), or
 *  `undefined` where the newest press did not end stopped, so the surface shows its own text for
 *  the refusal instead. `what` is what Dispatch has not confirmed and `made` what the write does
 *  once it lands, as `stoppedWriteNote` takes them; the method the Retry's sentence reads is the
 *  write's own, so no site states it a second time (`KeyedWriteResult.method`). */
export function stoppedNote<
  W extends { readonly error: unknown; readonly method: WriteMethod | undefined },
>(write: W, what: string, made: string): string | undefined {
  return endedStopped(write)
    ? stoppedWriteNote(what, made, { method: write.method, stopped: write.error })
    : undefined;
}

/** The sentence a surface shows for its write Dispatch refused, naming the press it refused -
 *  `what` it could not do, as `revoke your sign-in on devbox` - with Dispatch's own words of why
 *  after it where Dispatch gave any (`apiErrorMessage`). A surface whose one line speaks for many
 *  subjects says which one this way (`useRefusalOfSubject`). */
export function refusedNote(what: string, refusal: unknown): string {
  const why = apiErrorMessage(refusal, "");
  return why === "" ? `Couldn't ${what}.` : `Couldn't ${what} — ${why}`;
}

/**
 * Whether a surface shows the refusal of `write`'s newest press: while the subject that press was
 * about is still on the surface (`here`, handed the press's variables), and never after. A surface
 * drawn once for many subjects - a table of rows, a list under the reader's filters, the shell
 * under whoever is signed in - holds one refusal for all of them, and one that outlived its
 * subject would tell the reader something false about what is in front of them, its Retry a
 * press of something gone. So the moment `here` stops holding - the row removed or renamed, the
 * filter changed, another person signed in - the write lets that refusal go (`reset`), before the
 * screen is drawn, and it does not come back with the subject.
 */
export function useRefusalOfSubject<TVariables>(
  write: {
    readonly isError: boolean;
    readonly reset: () => void;
    readonly variables: TVariables | undefined;
  },
  here: (variables: TVariables) => boolean
): boolean {
  const gone = write.isError && write.variables !== undefined && !here(write.variables);
  const { reset } = write;
  useLayoutEffect(() => {
    if (gone) reset();
  }, [gone, reset]);
  return write.isError && !gone;
}

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
 * pending-writes indicator while it is out and, refused, with Retry and Discard (`Writes.run`) -
 * no Retry for a conflict only the reader can settle (`isConflict`), which its kind's surface may
 * take to settle with them (`WriteKind.claims`).
 * The mutation settles with Dispatch's answer - `null` for a landing without one - its refusal, or
 * `SendStoppedError` once the reader stops it, so the surface keeps its optimistic state until
 * then and rolls it back on anything but a landing.
 *
 * A press the reader stopped is no verdict: it may have reached Dispatch, which answers the key
 * of a `POST` or a `PATCH` with what it did there; a `PUT` or a `DELETE` sends no key, and is the
 * same change however often it is made. So the hook holds every press that ended stopped,
 * under its key, and that press belongs to the press that made it: the surface's own Retry
 * makes it again, through `retry` where the Retry is of the surface's newest press, or
 * through the handle `stopped` gives and `again` where more than one press is out at once. A
 * `mutate` or `mutateAsync` is a press the person made anew and always goes under a new key,
 * whatever its variables are: the hook never matches a press by its variables, by identity or by
 * value, so a later press of equal values - a title typed again, Approve after a change request -
 * is a write Dispatch decides, never one it answers with what the stopped press did there while
 * the person's own choice is dropped in silence. Where the stopped press had landed, the later
 * press is a second write: Dispatch refuses it where that would be wrong - a name the owner holds
 * now, an upload's base the document has moved past - and the person reads that refusal, and a
 * field ends at the value the person chose last. A Retry after Dispatch's own refusal is a press
 * of its own too, since Dispatch would answer the old key with that refusal.
 *
 * The shell's pending writes can make a stopped press again as well (`Writes.retry`). Once that
 * lands, the hook holds the press no longer and, where it is the press the hook made last
 * (`mutate`, `mutateAsync` or `again`), clears its error (`reset`), so the surface's note that
 * Dispatch has not confirmed it goes. No press of the hook awaited that landing, so none of the
 * surface's own callbacks run for it; the store reads every query again instead, as it does for
 * each landing of a plain write no press awaited (`plainWrite`, `Writes.#landed`). The press the
 * hook made last is not always the newest press React drew (`variables`): a press made just
 * before that landing is not drawn yet, and it keeps its own pending state and its refusal.
 *
 * A Retry (`retry`, `again`) runs the newest render's `write` or `request`, and its callbacks,
 * with the press's own variables. So a press carries every value they read - an upload's base
 * and summary (`UploadPress`), the ask a card's press is of (`AskPress`) - and its Retry sends
 * what its key covers, whatever the surface shows by then.
 *
 * TanStack runs each press as the hook made it (`KeyedPress`), the key with it, so the key goes
 * with its own press however many are out at once; the surface's callbacks and the result's
 * `variables` are the press's own variables, as from a `useMutation` of its own. It runs whether
 * or not the browser says it is online (`networkMode: "always"`): the write's own tries wait out a
 * network that is down, and a mutation TanStack paused offline would make none, never go late and
 * never offer Stop trying. A press that lands with no answer is kept as `unanswered` for the
 * surface to say so, until the next press or `forgetUnanswered`.
 *
 * A write given `holds` holds back the reads of those queries from its press until Dispatch has
 * taken it (`KeyedWriteOptions.holds`): TanStack counts the write as pending until its
 * `onSettled` has run, which may itself read those queries again, so the hold ends as `onSuccess`
 * or `onError` returns, if its event or its deadline has not ended it first, rather than when the
 * write settles.
 */
export function useKeyedWrite<TData, TVariables = void, TContext = unknown, D = null, K = D>({
  detail,
  endsWithSession,
  file,
  holds,
  inline,
  keyed,
  kind,
  label,
  method,
  onError,
  onMutate,
  onSettled,
  onSuccess,
  readsLate,
  request,
  target,
  write,
  ...options
}: KeyedWriteOptions<TData, TVariables, TContext, D, K>): KeyedWriteResult<
  TData,
  TVariables,
  TContext
> {
  const writes = useWrites();
  /** Every press the reader stopped that is neither made again nor landed by the shell's Retry,
   *  newest first. Each is reached by its own object alone: `again` of the handle `stopped` gave,
   *  or `retry` of the newest press. */
  const held = useRef<readonly Press<TVariables>[]>([]);
  /** The press the hook made last (`mutate`, `mutateAsync`, `again`), set as it is made, before
   *  TanStack tells React of it. */
  const newestMade = useRef<Press<TVariables> | undefined>(undefined);
  /** Every press whose run is out, by the key its attempts go out under, from the start of its
   *  run until it settles: the presses `lateOf` looks among. */
  const out = useRef(new Map<string, TVariables>());
  const [unanswered, setUnanswered] = useState<UnansweredLanding<TVariables> | undefined>(
    undefined
  );
  const forgetUnanswered = useCallback(() => setUnanswered(undefined), []);
  /** The method of the request a press of `variables` makes. */
  const methodOf = (variables: TVariables): WriteMethod =>
    write === undefined
      ? request(variables).method
      : typeof method === "function"
        ? method(variables)
        : method;
  const mutation = useMutation<TData | null, DefaultError, Press<TVariables>, TContext>({
    ...options,
    mutationFn: (press) => {
      const { key, variables } = press;
      setUnanswered(undefined);
      out.current.set(key, variables);
      const spec = {
        // A press names a kind and its record together, or neither (`KeyedWriteKind`): with
        // neither it is a plain write, whose record is `D`'s default, `null`.
        detail: (detail === undefined ? null : detail(variables)) as D,
        endsWithSession,
        file: file?.(variables),
        inline: typeof inline === "function" ? inline(variables) : inline,
        key,
        keyed,
        kind: (kind ?? plainWrite) as WriteKind<D, K>,
        label: label(variables),
        presentation: "indicator",
      } as const;
      return (
        write === undefined
          ? writes.run({ ...spec, request: request(variables), target: target(variables) })
          : writes.run({
              ...spec,
              method: methodOf(variables),
              write: (bounds) => write(variables, bounds),
            })
      ).then(
        (answer) => {
          out.current.delete(key);
          if (answer === null) setUnanswered({ at: Date.now(), variables });
          return answer;
        },
        (error: unknown) => {
          out.current.delete(key);
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
    ...(holds === undefined
      ? {}
      : {
          onMutate: (press: Press<TVariables>, context: MutationFunctionContext) => {
            pressHolds.set(context, holdBackReads(context.client, holds(press.variables)));
            return onMutate?.(press.variables, context) as TContext | Promise<TContext>;
          },
          onSuccess: (
            data: TData | null,
            press: Press<TVariables>,
            result: TContext,
            context: MutationFunctionContext
          ) => {
            try {
              return onSuccess?.(data, press.variables, result, context);
            } finally {
              endHold(context);
            }
          },
          onError: (
            error: DefaultError,
            press: Press<TVariables>,
            result: TContext | undefined,
            context: MutationFunctionContext
          ) => {
            try {
              return onError?.(error, press.variables, result, context);
            } finally {
              endHold(context, refusedByDispatch(error));
            }
          },
        }),
  });
  const { mutate: makePress, mutateAsync: makePressAsync } = mutation;
  const mutate = useCallback(
    (
      ...[variables, callbacks]: Parameters<
        UseMutateFunction<TData | null, DefaultError, TVariables, TContext>
      >
    ) => {
      const press = { key: newSendKey(), variables: variables as TVariables };
      newestMade.current = press;
      makePress(press, pressCallbacks(callbacks));
    },
    [makePress]
  );
  const mutateAsync = useCallback(
    (
      ...[variables, callbacks]: Parameters<
        UseMutateAsyncFunction<TData | null, DefaultError, TVariables, TContext>
      >
    ) => {
      const press = { key: newSendKey(), variables: variables as TVariables };
      newestMade.current = press;
      return makePressAsync(press, pressCallbacks(callbacks));
    },
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
        return false;
      }
      held.current = held.current.filter((each) => each !== made);
      newestMade.current = made;
      makePress(made);
      return true;
    },
    [makePress]
  );
  const newest = mutation.variables;
  // The newest press's write as the store holds it, by the key the press made it under, and the
  // writes ahead of it on its target's line, followed only where the surface reads it
  // (`readsLate`).
  const late = useLateIn(writes, readsLate === true ? newest?.key : undefined);
  // Every press out that is late, followed only where the surface reads it (`readsLate`), from
  // the newest press on and while any press is out.
  const lateOut = useLateKeys(
    writes,
    readsLate === true && (newest !== undefined || out.current.size > 0),
    () => [...out.current.keys()]
  );
  const lateOf = useCallback(
    (variables: TVariables) => {
      const keys = lateOut.split(" ");
      return [...out.current].some(([key, made]) => made === variables && keys.includes(key));
    },
    [lateOut]
  );
  // Built only once the newest press ended stopped, when its request went out: a press whose
  // request could not be built ended with that error instead, which building it here would throw.
  const newestMethod =
    newest !== undefined && mutation.error instanceof SendStoppedError
      ? methodOf(newest.variables)
      : undefined;
  const retry = useCallback(() => {
    if (newest !== undefined && !again(newest)) mutate(newest.variables);
  }, [again, mutate, newest]);
  // A held press the shell's Retry lands (the hook's doc says what follows): it is let go, and
  // where it is the press the hook made last, its error is cleared. That is read from the press as
  // it was made (`newestMade`) and not as React last drew it, so a press made just before the
  // landing, which React has not drawn yet, keeps its own state.
  const { reset } = mutation;
  useEffect(
    () =>
      writes.onOutcome((outcome) => {
        const { key } = outcome.write;
        if (outcome.kind !== "landed" || !held.current.some((press) => press.key === key)) return;
        held.current = held.current.filter((press) => press.key !== key);
        if (key === newestMade.current?.key) reset();
      }),
    [reset, writes]
  );
  // TanStack's result is a union over `status`, each member with the press it ran, and the
  // press's own variables stand in for it member by member, which the spread does not show.
  return {
    ...mutation,
    again,
    forgetUnanswered,
    late,
    lateOf,
    method: newestMethod,
    mutate,
    mutateAsync,
    retry,
    stopped,
    unanswered,
    variables: newest?.variables,
  } as KeyedWriteResult<TData, TVariables, TContext>;
}
