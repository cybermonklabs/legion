import { type Context, useCallback, useContext, useMemo, useState } from "react";

import type { Ask } from "../../api/types";
import { type KeyedWriteOptions, useKeyedWrite } from "../writes/useKeyedWrite";

/** What every press a card makes of an ask carries - a hand-on, an answer, a decline, a reply, a
 *  question back: the ask it is of. `retry()` runs the newest render's `write` and callbacks with
 *  the press's own values (`useKeyedWrite`), and a host may draw one card for one ask after
 *  another, so the ask a write is sent to, what it changes in the Inbox's read and the ask the
 *  Inbox holds are the press's, whichever ask the card shows. */
export interface AskPress {
  readonly ask: Ask;
}

/** The ask a press of a card's ask write is of (`useAskWrites`'s `askIdOf`). */
export function pressAsk({ ask }: AskPress): string {
  return ask.id;
}

/** The newest press of one ask's write of one kind in an Inbox, and what became of it: `out`
 *  until Dispatch decides it, then `landed` where the kind keeps a landed press (`keepsLanded`),
 *  or `failed` with the error Dispatch refused it with or the reader's stop. `at` is when the
 *  press was made. The next press of the ask takes its place. */
export type AskPressState<P> =
  | { readonly at: number; readonly press: P; readonly state: "landed" | "out" }
  | { readonly at: number; readonly failure: Error; readonly press: P; readonly state: "failed" };

/** One kind of an Inbox's ask writes - its snoozes, its hand-ons, its answers, its declines, its
 *  questions back or its replies - held by ask (`useAskWrites`). */
export interface AskWrites<P> {
  /** Lets go of the newest press of `askId` once Dispatch has decided it, its refusal with it.
   *  A press that is out stays: it is Dispatch's to decide. */
  readonly forget: (askId: string) => void;
  /** Lets go of the newest press of `askId` where it has landed. A press that is out or failed
   *  stays. The same function on every render. */
  readonly forgetLanded: (askId: string) => void;
  /** The asks whose newest press is out past a deadline (`KeyedWriteResult.lateOf`): still
   *  `out`, and tried under its key until Dispatch decides it or the reader stops it, but a
   *  surface that holds its controls while the press is out lets them go here. */
  readonly late: ReadonlySet<string>;
  /** The asks whose newest press is out or failed. The Inbox marks each one as the reader's own
   *  write (`writtenHere`) while it is here: a write marks its ask while it is pending or failed,
   *  so a row whose write Dispatch refused, or the reader stopped, that then leaves the list is
   *  the reader's own move too, and the open row's reading column moves on rather than saying
   *  it left. */
  readonly live: ReadonlySet<string>;
  /** The newest press of each ask this kind has written, by ask. */
  readonly newest: ReadonlyMap<string, AskPressState<P>>;
  /** The asks whose write is out. The Inbox keeps each one's row where the reader saw it until
   *  the write lands (`heldRow`). A refused or stopped write is not out, so the next read of the
   *  Inbox places its row. */
  readonly out: ReadonlySet<string>;
  /** Makes `press`. It is out from this call on, in the commit of the reader's own press, so the
   *  Inbox holds the row before the press's optimistic change reaches the cache. */
  readonly press: (press: P) => void;
  /** Makes the failed press of `askId` again: under its own key where the reader stopped it
   *  (`again`), else as a new press of the same values. Nothing where its newest press did not
   *  fail. */
  readonly retry: (askId: string) => void;
}

/** `entries` without the entry of `askId`. */
function without<V>(entries: ReadonlyMap<string, V>, askId: string): Map<string, V> {
  const next = new Map(entries);
  next.delete(askId);
  return next;
}

/**
 * One kind of an Inbox's ask writes, held by ask in the Inbox's own state rather than by the
 * control that pressed. A control can go while its write is out: the reading column's head and
 * card go whenever the reader moves the selection. So the write, its row's hold and its refusal
 * outlive the control, and every control drawn for the ask shows that ask's one write of this
 * kind, whichever control pressed it.
 *
 * One write hook makes the presses of every ask (`useKeyedWrite`, over `options`), so more than
 * one can be out at once. Each press carries the ask it is of (`askIdOf`), and a Retry runs the
 * newest render's `write` with the press's own values, so every byte a Retry sends rides in the
 * press. A refusal stays with its ask until the ask's next press, or until a control lets it go
 * (`forget`). `options.onError` runs before the press is decided failed, so a rollback is in the
 * cache before the Inbox lets go of the row, and the render that lets go of it draws the row as
 * the rollback left it.
 *
 * A press that lands is let go of as it lands, unless `keepsLanded` answers true for its ask:
 * then it stays, `landed`, until `forgetLanded` or the ask's next press. A kind whose surfaces
 * read nothing of a landed press keeps none, so its entries do not grow with each ask written
 * while the Inbox stays.
 *
 * `askIdOf` names the ask of a press; a module's own function, so it is the same on every render.
 */
export function useAskWrites<TData, P, TContext = unknown>(
  askIdOf: (press: P) => string,
  options: KeyedWriteOptions<TData, P, TContext>,
  keepsLanded?: (askId: string) => boolean
): AskWrites<P> {
  const [newest, setNewest] = useState<ReadonlyMap<string, AskPressState<P>>>(() => new Map());
  // Dispatch's decision on `press`: a landing (`failure` undefined) or a refusal or a stop. Where
  // a newer press of the same ask has taken this press's place, the decision changes nothing.
  const decide = (press: P, failure: Error | undefined) =>
    setNewest((current) => {
      const askId = askIdOf(press);
      const entry = current.get(askId);
      if (entry?.press !== press) return current;
      if (failure !== undefined) {
        return new Map(current).set(askId, { at: entry.at, failure, press, state: "failed" });
      }
      if (keepsLanded?.(askId) !== true) return without(current, askId);
      return new Map(current).set(askId, { at: entry.at, press, state: "landed" });
    });
  const { again, lateOf, mutate, stopped } = useKeyedWrite<TData, P, TContext>({
    ...options,
    readsLate: true,
    onError: (error, press, result, context) => {
      options.onError?.(error, press, result, context);
      decide(press, error);
    },
    onSuccess: (data, press, result, context) => {
      options.onSuccess?.(data, press, result, context);
      decide(press, undefined);
    },
  });
  const forgetLanded = useCallback(
    (askId: string) =>
      setNewest((current) =>
        current.get(askId)?.state === "landed" ? without(current, askId) : current
      ),
    []
  );
  return useMemo(() => {
    const out = new Set<string>();
    const late = new Set<string>();
    const live = new Set<string>();
    for (const [askId, { press, state }] of newest) {
      if (state !== "landed") live.add(askId);
      if (state !== "out") continue;
      out.add(askId);
      if (lateOf(press)) late.add(askId);
    }
    const holdOut = (press: P) =>
      setNewest((current) =>
        new Map(current).set(askIdOf(press), { at: Date.now(), press, state: "out" })
      );
    return {
      forget: (askId: string) =>
        setNewest((current) => {
          const state = current.get(askId)?.state;
          return state === undefined || state === "out" ? current : without(current, askId);
        }),
      forgetLanded,
      late,
      live,
      newest,
      out,
      press: (press: P) => {
        holdOut(press);
        mutate(press);
      },
      retry: (askId: string) => {
        const entry = newest.get(askId);
        if (entry?.state !== "failed") return;
        const { press } = entry;
        holdOut(press);
        const kept = stopped((variables) => variables === press);
        if (kept === undefined || !again(kept)) mutate(press);
      },
    };
  }, [again, askIdOf, forgetLanded, lateOf, mutate, newest, stopped]);
}

/** The writes of one kind of the Inbox around the caller, which provides every kind of its ask
 *  writes (`Inbox`). A list or a row that reads them outside an Inbox is a mistake in the code,
 *  so this throws for one. */
export function useInboxWrites<W>(context: Context<W | undefined>): W {
  const writes = useContext(context);
  if (writes === undefined) {
    throw new Error("An Inbox's ask writes are read outside an Inbox");
  }
  return writes;
}
