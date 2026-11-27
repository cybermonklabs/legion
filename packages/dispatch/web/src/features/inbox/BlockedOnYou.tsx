import { awaitsDecision, meInboxAsks } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { meInboxQuery, whoAmIQuery } from "../../api/queries";
import type { CredentialPendingRow, OpenAskRow } from "../../api/types";
import { useIsHuman } from "../../hooks/useIsHuman";
import { compareTimestamps, isTimestamp } from "../../lib/timestamps";
import {
  surfaceMutedStrongBg,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useCredentialRequests } from "../credentials/pending";
import { formatAskAge } from "./ask-age";
import { isMine } from "./sections";
import { isSnoozed } from "./snooze";

/** Open asks whose turn is a person's (`waiting_on === "human"`) and that the viewer has not
 *  deferred. Whose turn it is, not whose question it is: who answers it is the person the ask
 *  waits on, which `needsYou` below adds. An agent's progress note keeps its ask waiting on the
 *  agent, whoever replied last; a snooze keeps its ask off every "needs you" surface - the nav
 *  badge, the sidebar, the Inbox's Blocked on you control - until it returns, because
 *  deferring it is the viewer saying it does not need them now. A row that carries no snooze
 *  at all (an issue header's `open_asks`) is never deferred. */
export function waitingOnHuman<
  T extends Pick<OpenAskRow, "waiting_on"> & Partial<Pick<OpenAskRow, "snoozed_until">>,
>(asks: readonly T[]): T[] {
  return asks.filter((ask) => ask.waiting_on === "human" && !isSnoozed(ask));
}

/** The one rule for work waiting on the reader themselves: an open ask whose turn is a
 *  person's and that they have not deferred (`waitingOnHuman`), waiting on them
 *  (`isMine`, the same rule the list's own filter reads, which reads the server's
 *  `waiting_on_person`: whoever the ask was handed to, the assignee of its issue until
 *  somebody hands it on, and the default assignee of its project while the issue holds
 *  nobody - a question on a project document included, which reaches that project's default
 *  the same way). Every project names a default assignee, so every question is somebody's and
 *  a question nobody waits on is one an older server answered.
 *  Nobody is the reader until the login resolves, so an unknown viewer is waited on by
 *  nothing: a list taken over the whole inbox while the shell loads would not be theirs.
 *  Every count of such work - the badges, the Inbox's Blocked on you control, the project
 *  page's pill, the Agents page's first card number - reads this, so they all mean the same
 *  thing. */
export function waitingOnYou(
  asks: readonly OpenAskRow[],
  viewer: string | undefined
): OpenAskRow[] {
  if (viewer === undefined) return [];
  return waitingOnHuman(asks).filter((ask) => isMine({ ask, kind: "ask" }, viewer));
}

/** How much waits for the viewer, and since when. */
export interface NeedsYou {
  /** How many things wait for the viewer: asks and credential requests together. */
  readonly count: number;
  /** When the oldest of them began waiting, or undefined where nothing does. */
  readonly oldest: string | undefined;
}

/** What the Inbox switch's **Blocked on you** part is named and titled by, at every width: how
 *  many things wait for the viewer, and how long the oldest of them has waited; its name alone
 *  while a read it counts has not answered (`undefined`), which no number is drawn from. */
export function blockedOnYouText(needs: NeedsYou | undefined): string {
  if (needs === undefined) return "Blocked on you";
  const { count, oldest } = needs;
  const items = `${count} ${count === 1 ? "item" : "items"}`;
  return `Blocked on you: ${items}${oldest === undefined ? "" : `, oldest ${formatAskAge(oldest)}`}`;
}

/** Everything that waits for the viewer themselves - every ask `waitingOnYou` keeps, and every
 *  pending credential request they may decide, as its approver or as anyone signed in -
 *  counted, with when the oldest of them began waiting (undefined when nothing does). An expired
 *  credential request is listed until somebody dismisses it, but nobody can decide it any more,
 *  so it waits on nobody and is neither counted nor the oldest (`awaitsDecision`). The
 *  badges and the Inbox's own needs-you control read this one rule, so every count of work
 *  waiting on the reader means the same thing whatever the list on screen is filtered
 *  to, and another kind of waiting item is added here once. An ask is counted only once the
 *  login resolves - a number taken over the whole inbox while the shell loads would not be the
 *  reader's - which `waitingOnYou` already does; a credential request needs no login of the
 *  SPA's to scope it, because the list is already the caller's own: the browser may ask only
 *  `?approver=me` and Dispatch resolves it from the auth token, refusing any other value with
 *  `APPROVER_ME_ONLY` (`resolveApproverMe`,
 *  `packages/envoy/internal/dispatch/api/credential_requests.go`). So a request counts from the
 *  moment it is listed, as it did before the asks became the reader's own - a guard here would
 *  have suppressed requests that are the reader's by construction. A time `compareTimestamps`
 *  refuses still counts but is never the oldest: the broker's `requested_at` reaches here
 *  verbatim, and the badges render in the app shell, outside every page's error boundary, so a
 *  throw here would take down every page. */
export function needsYou(
  asks: readonly OpenAskRow[],
  credentialRequests: readonly CredentialPendingRow[],
  viewer: string | undefined
): NeedsYou {
  const since = [
    ...waitingOnYou(asks, viewer).map((ask) => ask.created_at),
    ...credentialRequests.filter(awaitsDecision).map((request) => request.requested_at),
  ];
  const oldest = since
    .filter(isTimestamp)
    .reduce<string | undefined>(
      (earlier, at) => (earlier === undefined || compareTimestamps(at, earlier) < 0 ? at : earlier),
      undefined
    );
  return { count: since.length, oldest };
}

/** The rail's and the compact top bar's `Needs you N`: the reader's own, from the one read's
 *  asks, which are the asks that wait on the reader and no other. They are selected once for each
 *  answer, not once a render: the shell and the rail each draw this. Undefined until each read it
 *  adds up has answered - the one read, the sign-in that says whose asks are the reader's, and
 *  the credential requests - and while a first read of any of them has failed, so the rail
 *  draws no number rather than a sum that counts an unread list as empty. A read that answered
 *  once keeps its answer through a later refresh that fails. */
export function useNeedsYouCount(): number | undefined {
  const human = useIsHuman();
  const own = useQuery({ ...meInboxQuery(), enabled: human, select: meInboxAsks });
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const credentials = useCredentialRequests();
  if (own.data === undefined || viewer === undefined || credentials.status !== "listed") {
    return undefined;
  }
  return needsYou(own.data, credentials.requests, viewer).count;
}

/** The project page's `Blocked on you · N` pill, linking to the Inbox: the reader's own
 *  questions over the project's rows, and no credential request, since a request belongs to no
 *  project. Absent while nothing waits on the reader. */
export function BlockedOnYou({
  asks,
  className,
  viewer,
}: {
  asks: readonly OpenAskRow[];
  className?: string;
  /** The signed-in person's lowercase login, whose questions this counts; nothing is counted
   *  until the caller's `/auth/whoami` read resolves. */
  viewer: string | undefined;
}): ReactNode {
  const count = waitingOnYou(asks, viewer).length;
  if (count === 0) return null;
  return (
    <Link
      className={`${className ?? ""} inline-flex min-h-11 items-center rounded-full px-3 text-xs font-medium md:min-h-9 md:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary}`}
      to="/"
    >
      Blocked on you · {count}
    </Link>
  );
}
