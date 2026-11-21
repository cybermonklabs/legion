import { awaitsDecision } from "@legion/contracts";
import { useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { api, apiErrorMessage } from "../../api/client";
import type { CredentialPendingRow } from "../../api/types";
import { heldPress } from "../../components/held-press";
import { LabelPill } from "../../components/Pill";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
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
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { brokerPage } from "./brokerPage";
import { CredentialSessionLines } from "./CredentialSessionLines";
import { useMachineCodePage } from "./capabilities";
import { type CredentialRequests, credentialPendingQuery } from "./pending";
import { credentialSessionNamesAnyone } from "./session";

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

/** One expired request's lines: the summary a pending row shows, marked "Expired", with a Dismiss
 *  button, and when its deadline passed (the line says only that it passed when the broker names
 *  no deadline). Nobody decided it before that deadline, and every decision of it is refused now,
 *  so it links nowhere - not to the broker's page, not to the machine-code page - and dismissing
 *  it is all that is left: the dismissal takes it out of every list for good, a shared request's
 *  included. Each row makes its own dismissal and shows its own refusal under it (the broker's
 *  message: `NOT_APPROVER`, `RECORD_PENDING`, `RECORD_TERMINAL`, `NOT_FOUND`), as
 *  `RevocableList` does for a revoke; a dismissal that lands and one refused both read the list
 *  again, since either answer says what the broker now holds. Dismiss is held (`heldPress`), not
 *  disabled, while its write is out and through the read that takes the row away, so it keeps the
 *  focus and the press is still the cause of the row's going (`causedByReader`); the submit guard
 *  drops a second press that lands before the hold is drawn. */
function ExpiredRowLines({ row }: { row: CredentialPendingRow }): ReactNode {
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const label =
    `Dismiss expired ${requestKindLabel(row).toLowerCase()} ${requestIdentifiers(row)}`.trimEnd();
  // The row shows a refused dismissal itself, under its own line. The press carries the request
  // it dismisses and the name it shows by, so a Retry and the words it ends with are of that
  // request whatever this row is drawn for by then.
  const dismiss = useKeyedWrite({
    inline: true,
    label: (press: { readonly label: string; readonly recordId: string }) => press.label,
    onError: () => {
      void queryClient.invalidateQueries({ queryKey: credentialPendingQuery().queryKey });
    },
    onSettled: () => submitGuard.release(),
    // Pending, and Dismiss held, through the read of the list that takes the row away. That read
    // settles either way, and a failed one lets the button go.
    onSuccess: () => queryClient.invalidateQueries({ queryKey: credentialPendingQuery().queryKey }),
    method: "POST",
    write: ({ recordId }, bounds) => api.dismissCredentialRequest(recordId, bounds),
  });

  return (
    <>
      <div className={rowLineClass}>
        <RequestSummary row={row} />
        <LabelPill selected>Expired</LabelPill>
        <button
          aria-label={label}
          className={secondaryButtonCompact}
          {...heldPress(dismiss.isPending, () =>
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
    </>
  );
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
 *  their own (LEGION-587's review, round 3). */
export function CredentialRequestsSection({
  credentials: { requests, status },
}: {
  credentials: CredentialRequests;
}): ReactNode {
  const { agents, isError, isPending, refilling } = useAgents(
    requests.some((row) => credentialSessionNamesAnyone(row.session)),
    true
  );
  if (status === "failed") {
    return <p className={dangerText}>Couldn't load credential requests.</p>;
  }
  if (requests.length === 0) {
    return null;
  }

  return (
    <section aria-labelledby="credential-requests-heading" className="mb-6">
      <h2
        className={`text-base font-semibold ${textMutedOnCanvas}`}
        id="credential-requests-heading"
      >
        Credential requests
      </h2>
      <ul className="mt-2 space-y-2">
        {requests.map((row) => (
          <li key={row.record_id}>
            {awaitsDecision(row) ? <PendingRowLine row={row} /> : <ExpiredRowLines row={row} />}
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
