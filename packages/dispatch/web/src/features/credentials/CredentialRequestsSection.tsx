import { awaitsDecision } from "@legion/contracts";
import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useLayoutEffect, useRef } from "react";
import { Link } from "react-router-dom";

import { api, apiErrorMessage } from "../../api/client";
import { invalidateWaitingForPaused } from "../../api/query-client";
import { type ListReadHold, useListReadBound } from "../../api/read-bound";
import type { CredentialPendingRow } from "../../api/types";
import { ARM_ON_PRESS, focusableIn, useFocusPlace } from "../../components/focus-place";
import { heldPress } from "../../components/held-press";
import { LabelPill } from "../../components/Pill";
import { useReleaseWhenLate, useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  dangerText,
  focusVisibleRing,
  linkHoverText,
  linkText,
  secondaryButtonCompact,
  textMutedOnCanvas,
} from "../../theme/classes";
import { useAgents } from "../conversation/useAgents";
import { Timestamp } from "../refs/Timestamp";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite, usePendingKeyedPress } from "../writes/useKeyedWrite";
import { CredentialSessionLines } from "./CredentialSessionLines";
import { brokerPage } from "./logic/brokerPage";
import { useMachineCodePage } from "./logic/capabilities";
import { credentialSessionNamesAnyone } from "./logic/session";
import { type CredentialRequests, credentialPendingQuery } from "./pending";

const rowLineClass = "flex flex-wrap items-center gap-2 text-sm";
const rowLinkClass = `${rowLineClass} rounded-lg outline-none focus-visible:ring-2 ${focusVisibleRing} ${linkText} ${linkHoverText}`;

/** What a request's kind is called on its line. */
function requestKindLabel(row: CredentialPendingRow): string {
  return row.kind === "launcher_credential" ? "Machine login" : "Secret request";
}

/** What a request names, as its line lists it. */
function requestIdentifiers(row: CredentialPendingRow): string {
  return (row.identifiers ?? []).join(", ");
}

/** What every request's first line shows, pending or expired: its kind, what it names, and when
 *  it was asked. */
function RequestSummary({ row }: { row: CredentialPendingRow }): ReactNode {
  return (
    <>
      <LabelPill>{requestKindLabel(row)}</LabelPill>
      <span>{requestIdentifiers(row)}</span>
      <Timestamp at={row.requested_at} className={textMutedOnCanvas} />
    </>
  );
}

/** One pending request's line. A machine (`launcher_credential`) record is decided only through
 *  the code-lookup route, so its line links to the code-entry page rather than trying to deep-link
 *  the specific pending request — and only while this deployment serves that page, which it does
 *  only while the broker serves the device grant (`useMachineCodePage`); with no such page the
 *  line is plain text, since there is nowhere to send the person. A secret request is decided on
 *  the secrets broker's own page, so its line is a plain link there, leaving Dispatch in the same
 *  tab; from a broker that names no page (one older than it), the line is the same summary with
 *  no link at all, since Dispatch decides no secret request of its own any more. */
function PendingRowLine({ row }: { row: CredentialPendingRow }): ReactNode {
  const machineCodePage = useMachineCodePage();
  const summary = <RequestSummary row={row} />;
  if (row.kind === "launcher_credential") {
    if (!machineCodePage) {
      return <span className="flex flex-wrap items-center gap-2 text-sm">{summary}</span>;
    }
    return (
      <Link className={rowLinkClass} to="/credentials/machine">
        {summary}
      </Link>
    );
  }
  const page = brokerPage(row.approve_url);
  if (page === null) {
    return <div className={rowLineClass}>{summary}</div>;
  }
  return (
    <a className={rowLinkClass} href={page}>
      {summary}
    </a>
  );
}

/** What the section says when its list failed to load, and what an expired row says before why
 *  the list read after its dismissal has not settled. */
const CREDENTIAL_REQUESTS_NOT_LOADED = "Couldn't load credential requests.";

/** One expired request's lines: the summary a pending row shows, marked "Expired", with a Dismiss
 *  button, and when its deadline passed (the line says only that it passed when the broker names
 *  no deadline). Nobody decided it before that deadline, and every decision of it is refused now,
 *  so it links nowhere - not to the broker's page, not to the machine-code page - and dismissing
 *  it is all that is left: the dismissal takes it out of every list for good, a shared request's
 *  included. Each row makes its own dismissal and shows its own refusal under it (the broker's
 *  message: `NOT_APPROVER`, `RECORD_PENDING`, `RECORD_TERMINAL`, `NOT_FOUND`), as
 *  `RevocableList` does for a revoke; a dismissal that lands and one refused both read the list
 *  again, since either answer says what the broker now holds. Every dismissal reads the one list,
 *  so the section holds every row's Dismiss through that read with one hold (`useListReadBound`),
 *  and this row says why that read has not settled only where its own dismissal made the latest
 *  one (`unsettled`). Dismiss is held (`heldPress`), not disabled, while its write is out and
 *  through the read that takes the row away, so it keeps the focus until the row goes, and the
 *  section hands it on then (`CredentialRequestsSection`); the submit guard drops a second press
 *  that lands before the hold is drawn. The dismissal goes under the request's name, so a row
 *  drawn again while it is out - the Inbox's Peek drawer closed and opened - holds Dismiss from
 *  that press until its deadline, as the row it was made on did (`usePendingKeyedPress`). */
function ExpiredRowLines({
  holdThrough,
  row,
  unsettled,
}: {
  /** The section's hold through its read of the list, which this row's landed dismissal makes as
   *  the request's it dismissed (`useListReadBound`). */
  holdThrough: ListReadHold;
  row: CredentialPendingRow;
  /** Why the section's read of the list has not settled, where this request's dismissal made the
   *  latest one, else undefined. */
  unsettled: string | undefined;
}): ReactNode {
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const label =
    `Dismiss expired ${requestKindLabel(row).toLowerCase()} ${requestIdentifiers(row)}`.trimEnd();
  // The row shows a refused dismissal itself, under its own line. The press carries the request
  // it dismisses and the name it shows by, so a Retry and the words it ends with are of that
  // request whatever this row is drawn for by then.
  const mutationKey = ["credential-dismiss", row.record_id];
  const dismissOut = usePendingKeyedPress(mutationKey);
  const dismiss = useKeyedWrite({
    inline: true,
    label: (press: { readonly label: string; readonly recordId: string }) => press.label,
    onError: () => {
      void queryClient.invalidateQueries({ queryKey: credentialPendingQuery().queryKey });
    },
    onSettled: () => submitGuard.release(),
    // Pending, and Dismiss held, through the read of the list that takes the row away. That read
    // settles either way, and a failed one lets the button go. A read that has not settled by the
    // request deadline (`REQUEST_DEADLINE_MS`) - one paused while the browser is offline - lets
    // it go then, and the row says why the list is not read yet (`useListReadBound`) until that
    // read settles or another dismissal makes the next one.
    // The read waits on a fetch the network had already paused, which that deadline ends too
    // (`invalidateWaitingForPaused`).
    onSuccess: (_result, { recordId }) =>
      holdThrough(
        recordId,
        invalidateWaitingForPaused(queryClient, credentialPendingQuery().queryKey)
      ),
    method: "POST",
    mutationKey,
    readsLate: true,
    write: ({ recordId }, bounds) => api.dismissCredentialRequest(recordId, bounds),
  });
  useReleaseWhenLate(submitGuard, dismiss.late);

  return (
    <>
      <div className={rowLineClass}>
        <RequestSummary row={row} />
        <LabelPill selected>Expired</LabelPill>
        <button
          aria-label={label}
          className={secondaryButtonCompact}
          {...heldPress(dismissOut.isPending && !dismissOut.late, () =>
            submitGuard.guard(() => dismiss.mutate({ label, recordId: row.record_id }))
          )}
          type="button"
        >
          Dismiss
        </button>
      </div>
      <p className={`mt-0.5 text-xs ${textMutedOnCanvas}`}>
        {row.expires_at === undefined ? (
          "Its deadline passed"
        ) : (
          <>
            Its deadline passed <Timestamp at={row.expires_at} />
          </>
        )}
      </p>
      {/* A dismissal the reader stopped may have been made, and Dismiss is a press of its own,
          under a key of its own: it never makes the stopped dismissal again. */}
      {dismiss.isError ? (
        <p className={`mt-0.5 text-xs ${dangerText}`}>
          {dismiss.error instanceof SendStoppedError
            ? stoppedWriteNote("the dismissal of this request", "made", {
                anew: "Pressing Dismiss again sends a new dismissal.",
              })
            : apiErrorMessage(dismiss.error, "Could not dismiss this request.")}
        </p>
      ) : null}
      {/* A list read that has not settled is said as the section says a list that failed to
          load, in the same line. */}
      {!dismiss.isError && unsettled !== undefined ? (
        <p className={dangerText}>
          {CREDENTIAL_REQUESTS_NOT_LOADED} {unsettled}.
        </p>
      ) : null}
    </>
  );
}

/** The control the section hands the focus to once `gone`, a request's row a dismissal took away,
 *  has left the list (`CredentialRequestsSection`): the first one the focus can reach
 *  (`focusableIn`) in the nearest row after it that is still listed - an expired request's Dismiss,
 *  since the broker lists the expired requests last - else in the nearest such row before it.
 *  `drawn` is the rows as the commit before drew them, which still holds `gone`. Null where no
 *  other row offers a control. */
function controlNear(drawn: readonly Element[], gone: Element): HTMLElement | null {
  const at = drawn.indexOf(gone);
  if (at === -1) return null;
  for (const row of [...drawn.slice(at + 1), ...drawn.slice(0, at).reverse()]) {
    const control = row.isConnected ? focusableIn(row)[0] : undefined;
    if (control !== undefined) return control;
  }
  return null;
}

/** The Inbox's supplementary credential-requests section, mounted above the ask sections, from the
 *  Inbox's own reading of the list (`useCredentialRequests`), which its Blocked on you control and
 *  its empty state read too. A pending request's line leads to where it is decided; an expired
 *  one's is marked, links nowhere and offers Dismiss (`ExpiredRowLines`), and the broker lists
 *  every pending row before the expired ones. A Dispatch with no secrets broker lists none, so the
 *  whole section hides silently - the one deliberate quiet path; a failure is surfaced only when no
 *  list has ever loaded (a later poll failing keeps showing the last list it held,
 *  `useCredentialRequests`'s `status` stays `listed`). Calls `useAgents` once for every row in
 *  the list (before either early return, so the hook always runs), rather than once per
 *  `CredentialSessionLines` instance, for every row's directory and machine; the shell's one
 *  poller (`AgentRegistryPoller`) refreshes that list, so N listed rows never phase N polls of
 *  their own (LEGION-587's review, round 3).
 *
 *  Every dismissal reads the one list again, so the section holds every row's Dismiss through that
 *  read with one hold (`useListReadBound`): a dismissal that lands forgets why an earlier one's
 *  read had not settled, whichever row made it, and only the row whose dismissal made the latest
 *  read says why that one has not. The read takes a dismissed row away, and with it the Dismiss
 *  that held the reader's focus, which the browser would leave on the page's body. So the section
 *  hands that focus on itself (`useFocusPlace`), counted in the row it stood in: to the next
 *  listed request's Dismiss, else to the control of the row before it (`controlNear`), else, with
 *  no other row left, back over the page from the place the row left, as every hand-off walks. The
 *  press arms that hand-off at the section's root (`ARM_ON_PRESS`), and the hold through the read
 *  keeps the press the cause of the row's going. */
export function CredentialRequestsSection({
  credentials: { requests, status },
}: {
  credentials: CredentialRequests;
}): ReactNode {
  const { agents, isError, isPending, refilling } = useAgents(
    requests.some((row) => credentialSessionNamesAnyone(row.session)),
    true
  );
  const [unsettledOf, holdThrough] = useListReadBound(credentialPendingQuery().queryKey);
  const list = useRef<HTMLUListElement>(null);
  // The rows as the latest commit drew them, which the hand-off reads for the row a commit after
  // it took away (`controlNear`).
  const drawn = useRef<readonly Element[]>([]);
  const noteFocus = useFocusPlace(
    (element) => {
      const row = element.closest("li");
      return row !== null && row.parentElement === list.current ? row : null;
    },
    (row) => controlNear(drawn.current, row)
  );
  // After the hand-off's own check, which runs first in the same commit, so that check reads the
  // rows as the commit before drew them, the row this one took away among them.
  useLayoutEffect(() => {
    drawn.current = list.current === null ? [] : [...list.current.children];
  });
  if (status === "failed") {
    return <p className={dangerText}>{CREDENTIAL_REQUESTS_NOT_LOADED}</p>;
  }
  if (requests.length === 0) {
    return null;
  }

  return (
    <section
      aria-labelledby="credential-requests-heading"
      className="mb-6"
      {...ARM_ON_PRESS}
      onFocus={noteFocus}
    >
      <h2
        className={`text-base font-semibold ${textMutedOnCanvas}`}
        id="credential-requests-heading"
      >
        Credential requests
      </h2>
      <ul className="mt-2 space-y-2" ref={list}>
        {requests.map((row) => (
          <li key={row.record_id}>
            {awaitsDecision(row) ? (
              <PendingRowLine row={row} />
            ) : (
              <ExpiredRowLines
                holdThrough={holdThrough}
                row={row}
                unsettled={unsettledOf(row.record_id)}
              />
            )}
            <div className="mt-0.5 flex flex-col gap-0.5 text-xs">
              <CredentialSessionLines
                agents={agents}
                isError={isError}
                isPending={isPending}
                refilling={refilling}
                session={row.session}
              />
            </div>
          </li>
        ))}
      </ul>
    </section>
  );
}
