import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import {
  type Context,
  createContext,
  type RefObject,
  useCallback,
  useContext,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import type { WriteBounds, WriteMethod } from "../../api/client";
import { inboxQuery } from "../../api/queries";
import type { InboxRow } from "../../api/types";
import { askWritten } from "../../api/write-events";
import { SendStoppedError } from "../writes/keyed-write";
import { type KeyedWriteOptions, useKeyedWrite } from "../writes/useKeyedWrite";
import { type AskPress, pressAsk, useAskWrites } from "./ask-writes";

/** The rows the Inbox read held before the write took its ask's row out, kept so a refused write
 *  puts them back. Undefined where the write left the Inbox alone - a change to an answer the
 *  server has already recorded, whose ask has no open row there - and so kept nothing. */
type InboxRowsBefore = { readonly rows: InboxRow[] | undefined } | undefined;

/** What an answer, a decline, a question back or a reply is written with, beyond what the write
 *  itself does with the Inbox's row. */
export interface OutstandingAnswerWriteOptions<TData, TVariables extends AskPress> {
  /** What the shell's pending-writes indicator names this write, in the reader's words. */
  readonly label: (variables: TVariables) => string;
  /** The method of the request `write` makes (`KeyedWriteMade`). */
  readonly method: WriteMethod;
  readonly write: (variables: TVariables, bounds: WriteBounds) => Promise<TData>;
  /** Whether a press takes its ask's row out of the Inbox: false for one that leaves it alone.
   *  Read as each press goes - for its hold on the Inbox's reads, then for the removal, in the
   *  one step - so a refusal restores exactly what that press took out. Every press does by
   *  default. */
  readonly removesRow?: (variables: TVariables) => boolean;
  /** Called after the row is back, so a refusal the surface reads is read on the restored list. */
  readonly onError?: (error: Error, variables: TVariables, before: InboxRowsBefore) => void;
  readonly onSettled?: () => void;
  readonly onSuccess?: (data: TData | null, variables: TVariables) => void;
}

/** An answer's, a decline's, a question back's or a reply's write as the surface that makes it
 *  reads it: the part of `useKeyedWrite`'s result these surfaces read, of the newest press. */
export interface OutstandingAnswerWrite<TVariables> {
  readonly error: Error | null;
  readonly isError: boolean;
  readonly isPending: boolean;
  readonly isSuccess: boolean;
  /** The method of the newest press's request where the reader stopped it, else undefined. */
  readonly method: WriteMethod | undefined;
  readonly mutate: (variables: TVariables) => void;
  readonly reset: () => void;
  readonly retry: () => void;
  /** When the newest press was made, or 0 before any press. */
  readonly submittedAt: number;
  readonly variables: TVariables | undefined;
}

/**
 * What an answer, a decline, a question back or a reply is written with, by a surface's own write
 * and by an Inbox's alike, around what the surface writes it with (`surfaceOf`, for each press).
 * A write that takes its ask's row out of the Inbox's read (`removesRow`: an answer, a decline)
 * takes it out the moment it goes, puts it back where the server refuses it, and holds the
 * Inbox's reads until Dispatch has taken it (`holds`: its answer, or the ask's `ask.answered` or
 * `ask.resolved` event, and at most `SEND_DEADLINE_MS`): one asked before could be answered with
 * the row still in it. The Inbox lists the asks other people open, so the hold ends on the event
 * rather than wait out an answer that is late. A write that leaves the row alone - a change to a
 * recorded answer, a question back, a reply - takes nothing out and holds no read. The write is
 * inline: the surface shows a refusal itself, over what the reader wrote, so the shell's
 * indicator carries the write only while it is out.
 */
function outstandingAnswerWrite<TData, TVariables extends AskPress>(
  queryClient: QueryClient,
  surfaceOf: (press: TVariables) => OutstandingAnswerWriteOptions<TData, TVariables>
): KeyedWriteOptions<TData, TVariables, InboxRowsBefore> {
  const removes = (press: TVariables) => surfaceOf(press).removesRow?.(press) ?? true;
  return {
    holds: (press) => ({
      endsOn: askWritten(press.ask.id, ["ask.answered", "ask.resolved"]),
      keys: removes(press) ? [inboxQuery().queryKey] : [],
    }),
    inline: true,
    label: (press) => surfaceOf(press).label(press),
    method: (press) => surfaceOf(press).method,
    write: (press, bounds) => surfaceOf(press).write(press, bounds),
    onMutate: async (press) => {
      if (!removes(press)) return undefined;
      await queryClient.cancelQueries({ queryKey: inboxQuery().queryKey });
      const rows = queryClient.getQueryData<InboxRow[]>(inboxQuery().queryKey);
      queryClient.setQueryData<InboxRow[]>(inboxQuery().queryKey, (current) =>
        current?.filter((row) => row.id !== press.ask.id)
      );
      return { rows };
    },
    onError: (error, press, before) => {
      if (before !== undefined) {
        queryClient.setQueryData(inboxQuery().queryKey, before.rows);
        void queryClient.invalidateQueries({ queryKey: inboxQuery().queryKey });
      }
      surfaceOf(press).onError?.(error, press, before);
    },
    onSettled: (_data, _error, press) => surfaceOf(press).onSettled?.(),
    onSuccess: (data, press) => surfaceOf(press).onSuccess?.(data, press),
  };
}

/** What a press of one of these writes is written with, as the surface it belongs to last drew it
 *  (`OutstandingAnswerWriteOptions`): a ref the surface keeps current. The Inbox's writes hold
 *  presses of every surface of one kind, each of its own variables, so they hold this without
 *  them. */
export type PressSurface = RefObject<OutstandingAnswerWriteOptions<unknown, AskPress>>;

/** What the newest write of one kind of one ask in an Inbox is doing, as every surface drawn for
 *  the ask and the ask's row show it. */
export type OutstandingAnswerShown = Omit<
  OutstandingAnswerWrite<AskPress>,
  "mutate" | "reset" | "retry"
>;

/** Every write of one kind the reader makes from a card in one Inbox - its answers, its declines,
 *  its questions back or its replies (`useOutstandingAnswerWrites`). */
export interface OutstandingAnswerWrites {
  /** Draws `surface` for `askId` until the returned function is called: a press of the ask that
   *  lands or is refused while its own surface is gone is the drawn surface's to show. */
  readonly draw: (askId: string, surface: PressSurface) => () => void;
  /** Lets go of the newest press of `askId` once Dispatch has decided it. */
  readonly forget: (askId: string) => void;
  /** The newest press of `askId`. Every surface drawn for the ask shows this one write. */
  readonly of: (askId: string) => OutstandingAnswerShown;
  /** The asks whose write is out. The Inbox keeps each one's row where the reader saw it, if it
   *  drew one, until the write lands (`heldRow`). A refused or stopped write is not out, so the
   *  next read of the Inbox places its row. */
  readonly out: ReadonlySet<string>;
  /** Makes `press` for `surface`. */
  readonly press: (press: AskPress, surface: PressSurface) => void;
  /** Makes the failed press of `askId` again, for `surface`. */
  readonly retry: (askId: string, surface: PressSurface) => void;
}

/**
 * Every write of one kind the reader makes from a card in one Inbox - its answers, its declines,
 * its questions back or its replies - held by ask in the Inbox's own state (`useAskWrites`), as
 * each row's snooze is. An answer or a decline takes the ask's row out of the Inbox's read the
 * moment it goes, and a question back hands the ask's turn to the asker, which a read of the
 * Inbox can show before Dispatch has answered the write. The card that pressed can go before the
 * server answers: the reading column's card goes whenever the reader moves the selection. So the
 * Inbox keeps the row where the reader saw it while the write is out, and the write and its
 * refusal outlive the card: the card drawn for the ask again shows them, with its Retry, and the
 * ask's row says what the write is doing (`AskShortRow`, and for a reply the row of the ask's
 * thread, `ThreadShortRow`).
 *
 * A press is written, and its landing or refusal handled, with the options of a surface
 * (`PressSurface`): the surface that made or last retried it, while that surface is drawn for
 * its ask; else a surface drawn for its ask where one is (`draw`), so that card's state follows
 * the write; else the surface that made or last retried it, as it last drew, whose callbacks
 * then change only what outlives it - a draft, a read.
 *
 * A landed press stays only while a surface is drawn for its ask, which reads its landing
 * (`isSuccess`, `submittedAt`). One that lands with none drawn is let go of as it lands, and one
 * kept is let go of once the last surface drawn for its ask goes.
 */
export function useOutstandingAnswerWrites(): OutstandingAnswerWrites {
  const queryClient = useQueryClient();
  // The surface that made or last retried each press, and the surfaces drawn for each ask.
  const [made] = useState(() => new WeakMap<AskPress, PressSurface>());
  const [drawn] = useState(() => new Map<string, Set<PressSurface>>());
  const surfaceOf = (press: AskPress): OutstandingAnswerWriteOptions<unknown, AskPress> => {
    const maker = made.get(press);
    const shown = drawn.get(press.ask.id);
    const surface =
      maker !== undefined && shown?.has(maker) === true
        ? maker
        : (shown?.values().next().value ?? maker);
    if (surface === undefined) {
      throw new Error("An ask write was pressed with no surface to write it");
    }
    return surface.current;
  };
  const writes = useAskWrites(pressAsk, outstandingAnswerWrite(queryClient, surfaceOf), (askId) =>
    drawn.has(askId)
  );
  const { forgetLanded } = writes;
  const draw = useCallback(
    (askId: string, surface: PressSurface) => {
      drawn.set(askId, (drawn.get(askId) ?? new Set()).add(surface));
      return () => {
        const surfaces = drawn.get(askId);
        surfaces?.delete(surface);
        if (surfaces?.size !== 0) return;
        drawn.delete(askId);
        forgetLanded(askId);
      };
    },
    [drawn, forgetLanded]
  );
  return useMemo(
    () => ({
      draw,
      forget: writes.forget,
      of: (askId: string): OutstandingAnswerShown => {
        const entry = writes.newest.get(askId);
        const failure = entry?.state === "failed" ? entry.failure : undefined;
        return {
          error: failure ?? null,
          isError: failure !== undefined,
          isPending: entry?.state === "out",
          isSuccess: entry?.state === "landed",
          method:
            failure instanceof SendStoppedError && entry !== undefined
              ? made.get(entry.press)?.current.method
              : undefined,
          submittedAt: entry?.at ?? 0,
          variables: entry?.press,
        };
      },
      out: writes.out,
      press: (press: AskPress, surface: PressSurface) => {
        made.set(press, surface);
        writes.press(press);
      },
      retry: (askId: string, surface: PressSurface) => {
        const entry = writes.newest.get(askId);
        if (entry?.state === "failed") made.set(entry.press, surface);
        writes.retry(askId);
      },
    }),
    [draw, made, writes]
  );
}

/** The reader's answers in the Inbox around a card (`useOutstandingAnswerWrites`). Each Inbox
 *  provides its own; with no provider a card makes its own write. */
export const AskAnswerWritesContext = createContext<OutstandingAnswerWrites | undefined>(undefined);

/** The reader's declines in the Inbox around a card, as `AskAnswerWritesContext` is for answers. */
export const AskDeclineWritesContext = createContext<OutstandingAnswerWrites | undefined>(
  undefined
);

/** The reader's questions back in the Inbox around a card (**Ask back**, **Ask back instead**), as
 *  `AskAnswerWritesContext` is for answers. */
export const AskClarificationWritesContext = createContext<OutstandingAnswerWrites | undefined>(
  undefined
);

/** The reader's replies under a settled ask in the Inbox around a reply composer
 *  (`AskReplyComposer`), as `AskAnswerWritesContext` is for answers. */
export const AskReplyWritesContext = createContext<OutstandingAnswerWrites | undefined>(undefined);

/**
 * A surface's own answer, decline or question back, made outside an Inbox - the issue page's
 * Conversation, a document's margin and its ask block, the Answers page: an answer or a decline
 * takes the ask's row out of the Inbox's read the moment it goes and puts it back where the
 * server refuses it (`outstandingAnswerWrite`). It lives as long as the surface: nothing there
 * takes the row out from under it.
 */
export function useOwnAnswerWrite<TData, TVariables extends AskPress>(
  options: OutstandingAnswerWriteOptions<TData, TVariables>
): OutstandingAnswerWrite<TVariables> {
  return useKeyedWrite(outstandingAnswerWrite(useQueryClient(), () => options));
}

/**
 * The Inbox's write of one kind of `askId` as a surface drawn for that ask shows it, from the
 * writes of that kind (`inbox`) the Inbox provides; undefined outside an Inbox, where `inbox` is
 * undefined.
 *
 * The Inbox's write is reached through the context it provides, since the card can be the
 * document's own - a question written as a decision block, drawn by `ProofDocument` - which
 * takes no prop of the Inbox's. The surface shows the newest press of the ask it is drawn for,
 * whichever surface made it, and makes its own presses with `options`. The Inbox holds the row
 * from the reader's own press, in that press's commit, so the commit the removal makes still
 * draws the row; and a card drawn for the ask after the reader moved away shows the write's
 * `…ing`, or its refusal and its Retry.
 */
export function useInboxAnswerWrite<TData, TVariables extends AskPress>(
  inbox: OutstandingAnswerWrites | undefined,
  askId: string,
  options: OutstandingAnswerWriteOptions<TData, TVariables>
): OutstandingAnswerWrite<TVariables> | undefined {
  // The Inbox's writes hold the presses of every surface of this kind, each made of that
  // surface's own variables, so a press of this surface's is handed back as its own type, and
  // this surface's options are handed over without it.
  const erased = options as unknown as OutstandingAnswerWriteOptions<unknown, AskPress>;
  const surface = useRef(erased);
  useLayoutEffect(() => {
    surface.current = erased;
  });
  const draw = inbox?.draw;
  useLayoutEffect(() => draw?.(askId, surface), [askId, draw]);
  if (inbox === undefined) return undefined;
  const shown = inbox.of(askId);
  return {
    ...shown,
    mutate: (variables) => inbox.press(variables, surface),
    reset: () => inbox.forget(askId),
    retry: () => inbox.retry(askId, surface),
    variables: shown.variables as TVariables | undefined,
  };
}

/**
 * The reader's own answer, or question back, to an ask, inside an Inbox and outside it alike: the
 * Inbox's write of `askId` of the kind `context` provides, where the card is drawn under one
 * (`useInboxAnswerWrite`), else the card's own (`useOwnAnswerWrite`). The answer form builds
 * `options` from its own state inside `useAskAnswerForm`, which `AskCard` calls at its top, so
 * the form cannot pick one of the two by the component it draws, as `DeclineControl` does. It
 * makes its own write inside an Inbox too, and that write makes no press there.
 */
export function useOutstandingAnswerWrite<TData, TVariables extends AskPress>(
  context: Context<OutstandingAnswerWrites | undefined>,
  askId: string,
  options: OutstandingAnswerWriteOptions<TData, TVariables>
): OutstandingAnswerWrite<TVariables> {
  const own = useOwnAnswerWrite(options);
  return useInboxAnswerWrite(useContext(context), askId, options) ?? own;
}
