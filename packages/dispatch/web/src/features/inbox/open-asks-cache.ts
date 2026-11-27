/**
 * The optimistic writes over the reads of the open asks (`openAsksKey`): every project's read the
 * Inbox, the margin, the Agents page and the Machines page share, and each project's own. Every
 * write that moves an open ask on the press - a hand-on, a snooze, an answer or a decline -
 * moves it in each of them as well as in the one read (`one-read-cache.ts`), puts each back
 * where the server refuses the write, and reads them again once the server has decided it. The
 * helpers over both at once (`inboxAsksKeys`, `cancelInboxAsks`, `inboxAsksSnapshot`,
 * `patchInboxAsks`, `restoreInboxAsks`) take each step in the order every such write takes it.
 *
 * Nothing here is a hook, so the writes call it from their own callbacks.
 */

import type { QueryClient, QueryKey } from "@tanstack/react-query";

import { meInboxQuery, openAsksKey } from "../../api/queries";
import type { OpenAskRow } from "../../api/types";
import {
  cancelMeInbox,
  type MeInboxSnapshot,
  meInboxSnapshot,
  patchMeInboxAsks,
  restoreMeInbox,
} from "./one-read-cache";

/** Every cached read of the open asks as it stood before a write moved it, for the rollback. */
export type OpenAsksSnapshot = readonly (readonly [QueryKey, OpenAskRow[] | undefined])[];

/** The key of every read of the open asks the cache holds now, which a write's hold names one by
 *  one (`holdBackReads` holds exact keys). */
export function openAsksKeys(queryClient: QueryClient): QueryKey[] {
  return queryClient
    .getQueryCache()
    .findAll({ queryKey: openAsksKey })
    .map((query) => query.queryKey);
}

/** Every cached read of the open asks, for a write to put back. */
export function openAsksSnapshot(queryClient: QueryClient): OpenAsksSnapshot {
  return queryClient.getQueriesData<OpenAskRow[]>({ queryKey: openAsksKey });
}

/** Cancels every read of the open asks in flight, so none lands over the optimistic write. */
export function cancelOpenAsks(queryClient: QueryClient): Promise<void> {
  return queryClient.cancelQueries({ queryKey: openAsksKey });
}

/** Leaves each row of every cached read of the open asks as `edit` leaves it: the row it returns,
 *  or none where it returns undefined. */
export function patchOpenAsks(
  queryClient: QueryClient,
  edit: (row: OpenAskRow) => OpenAskRow | undefined
): void {
  queryClient.setQueriesData<OpenAskRow[]>({ queryKey: openAsksKey }, (current) =>
    current?.flatMap((row) => {
      const next = edit(row);
      return next === undefined ? [] : [next];
    })
  );
}

/** Puts every read `snapshot` holds back as it was. */
export function restoreOpenAsks(
  queryClient: QueryClient,
  snapshot: OpenAsksSnapshot | undefined
): void {
  for (const [key, data] of snapshot ?? []) {
    queryClient.setQueryData(key, data);
  }
}

/** Reads every read of the open asks again. */
export function invalidateOpenAsks(queryClient: QueryClient): Promise<void> {
  return queryClient.invalidateQueries({ queryKey: openAsksKey });
}

/** Every cached read an Inbox ask row is drawn from as it stood before a write moved the row: the
 *  reads of the open asks and those of the one read, for the rollback. */
export interface InboxAsksSnapshot {
  readonly openAsks: OpenAsksSnapshot;
  readonly oneRead: MeInboxSnapshot;
}

/** The key of every read an Inbox ask row is drawn from: each read of the open asks the cache holds
 *  now, then the one read's Inbox answer, which a write's hold names one by one. */
export function inboxAsksKeys(queryClient: QueryClient): QueryKey[] {
  return [...openAsksKeys(queryClient), meInboxQuery().queryKey];
}

/** Cancels every read of the open asks and of the one read in flight, so none lands over the
 *  optimistic write. */
export function cancelInboxAsks(queryClient: QueryClient): Promise<unknown> {
  return Promise.all([cancelOpenAsks(queryClient), cancelMeInbox(queryClient)]);
}

/** Every cached read of the open asks, then every cached read of the one read, for a write to put
 *  back (`restoreInboxAsks`). */
export function inboxAsksSnapshot(queryClient: QueryClient): InboxAsksSnapshot {
  const openAsks = openAsksSnapshot(queryClient);
  return { openAsks, oneRead: meInboxSnapshot(queryClient) };
}

/** Leaves each ask row of the open asks' reads, then of the one read's, as `edit` leaves it; each
 *  row of `joined` joins the reader's own Inbox answer where it holds none (`patchMeInboxAsks`). */
export function patchInboxAsks(
  queryClient: QueryClient,
  edit: (row: OpenAskRow) => OpenAskRow | undefined,
  joined?: readonly OpenAskRow[]
): void {
  patchOpenAsks(queryClient, edit);
  patchMeInboxAsks(queryClient, edit, joined);
}

/** Puts every read `snapshot` holds back as it was: the open asks' reads, then the one read's. */
export function restoreInboxAsks(
  queryClient: QueryClient,
  snapshot: InboxAsksSnapshot | undefined
): void {
  restoreOpenAsks(queryClient, snapshot?.openAsks);
  restoreMeInbox(queryClient, snapshot?.oneRead);
}
