import { signInScopes } from "@legion/contracts";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import type { DecisionResponse, RecordResponse, StateResponse } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  dangerText,
  inputClasses,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { signInScopeStartsTicked, signInScopesText } from "../settings/dispatch-scope-choices";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { CredentialDecisionButtons } from "./CredentialDecisionButtons";
import { CredentialRecordFacts } from "./CredentialRecordFacts";
import { machineLoginsQuery, machineName } from "./logic/machineLogins";
import { MachineLoginsSection } from "./MachineLoginsSection";

/** What the login's decision was, in place of the buttons that decide it: one just made on this
 *  page, or the one a looked-up record already carries. A service's approved login (the Legion
 *  daemon's) starts worker pods as the service, where a person's machine starts sessions as them.
 *  A Dispatch sign-in names its reason beside its host, where it gave one, and the scopes approved
 *  where this page approved it; a record looked up once decided names only the scopes it asked
 *  for (`RecordResponse.dispatch_scopes`), so the line sends the person to the machine logins
 *  list, which names the approved ones. */
function MachineLoginDecision({
  credentialId,
  event,
  host,
  reason,
  service,
  signIn,
}: {
  credentialId: string | null;
  event: string;
  host: string;
  reason: string;
  service: string | null;
  signIn: { readonly asked: readonly string[]; readonly approved: readonly string[] | null } | null;
}): ReactNode {
  // The reason the request gave, as it typed it: the reading list's
  // `credential-request-reason`.
  const named = `${machineName({ host, service })}${reason === "" ? "" : ` (${reason})`}`;
  return (
    <div className="space-y-1 text-sm">
      <p className={`font-medium ${textPrimaryOnCanvas}`}>
        {event !== "approved" ? (
          <>
            <span className="capitalize">{event}</span>. {host} is not{" "}
            {signIn === null ? "logged in" : "signed in to Dispatch"}.
          </>
        ) : signIn === null ? (
          `Approved. ${machineName({ host, service })} can start ${service ? `worker pods as ${service}` : "agent sessions as you"}.`
        ) : signIn.approved === null ? (
          `Approved. ${named} is signed in to Dispatch. It asked for ${signInScopesText(signIn.asked)}; Machine logins below names the scopes approved.`
        ) : (
          `Approved. ${named} is signed in to Dispatch with ${signInScopesText(signIn.approved)}.`
        )}
      </p>
      {credentialId === null ? null : (
        <p className={textMutedOnCanvas}>Credential {credentialId}</p>
      )}
    </div>
  );
}

/** What an Approve press sends, read from the page when it is pressed: the record and the code it
 *  was looked up by, and the scopes ticked, null for a machine login, which sends none. */
interface ApprovalPress {
  readonly recordId: string;
  readonly code: string;
  readonly scopes: readonly string[] | null;
}

/**
 * The machine-login page: `agent-secrets machine login` prints an 8-character code on the
 * machine, and the operator types it here. A service, such as the Legion daemon, logs in with its
 * own Kubernetes token and prints no code; a service's login an older broker opened can still be
 * looked up here, and only denied. Decision 13 of the shared broker contract:
 * only this code-lookup route selects a
 * `launcher_credential` record, and deciding it sends the same code again, so this is the one
 * place a machine record gets Approve/Deny buttons, and only while the broker reads it as
 * pending. A looked-up login already decided shows its decision, and so does one decided here, in
 * their place, so a second click never reaches the broker's already-decided refusal. A login whose
 * deadline passed before anybody decided it reads as expired, before the broker's sweep ends it
 * and after, and the broker refuses every decision of it: it shows that in one line, with no
 * buttons. Below it, the machine logins the viewer may revoke that can still reach a secret:
 * their own machines', their own pods' and every service's.
 *
 * A Dispatch sign-in is looked up and decided here too. Its boxes start ticked for the scopes it
 * asked for that the page draws in its own words, but `p0`, which starts unticked so the person
 * ticks it for that sign-in themselves; a word the page does not know is drawn as itself,
 * unticked. Approve sends only ticked boxes of scopes the record asked for, so the page never
 * sends a scope it did not show. The list below marks each sign-in with its approved scopes; it
 * starts no agent session and reads no secret.
 */
export function MachineLoginPage(): ReactNode {
  const [code, setCode] = useState("");
  const queryClient = useQueryClient();
  const [ticked, setTicked] = useState<readonly string[]>([]);
  const submitGuard = useSubmitGuard();

  // A lookup and a decision are the form's own: it shows a refusal, beside the code it names.
  const lookup = useKeyedWrite<RecordResponse, string>({
    inline: true,
    label: () => "Lookup of a machine login code",
    onSettled: () => submitGuard.release(),
    // A sign-in's boxes start ticked for the asked scopes the page knows, but p0: it and a scope
    // drawn as a bare word start unticked (signInScopeStartsTicked).
    onSuccess: (lookedUp) =>
      setTicked(
        (lookedUp === null ? null : signInScopes(lookedUp))?.filter(signInScopeStartsTicked) ?? []
      ),
    method: "POST",
    write: (lookupCode, bounds) => api.lookupMachineCredential(lookupCode, bounds),
  });
  // A lookup Dispatch answered with nothing it kept shows nothing until it is looked up again.
  const record = lookup.data ?? undefined;
  // The code the shown record was looked up by, not whatever the field holds now.
  const lookedUpCode = lookup.variables ?? "";

  // Each is typed by its answer, which its callbacks read.
  // An approval carries the record, the code and the scopes as they were when Approve was pressed
  // (`ApprovalPress`), so its request and the view of its answer are the press's own.
  const approve = useKeyedWrite<DecisionResponse, ApprovalPress>({
    method: "POST",
    write: (press, bounds) =>
      api.approveCredentialRecord(
        press.recordId,
        { code: press.code, ...(press.scopes === null ? {} : { scopes: [...press.scopes] }) },
        bounds
      ),
    inline: true,
    label: () => "Approval of a machine login",
    onSettled: () => submitGuard.release(),
    onSuccess: (decided, press) => {
      void queryClient.invalidateQueries({ queryKey: ["credential-pending"] });
      void queryClient.invalidateQueries({ queryKey: machineLoginsQuery().queryKey });
      // Decided, with no answer to say how: the record is looked up again for its decision.
      if (decided === null) lookup.mutate(press.code);
    },
  });
  // Approve as the buttons press it: the press reads the shown record, its code and the boxes
  // ticked now, each a scope the record asked for.
  const askedScopes = record === undefined ? null : signInScopes(record);
  const approval = {
    error: approve.error,
    isError: approve.isError,
    isPending: approve.isPending,
    mutate: () =>
      approve.mutate({
        code: lookedUpCode,
        recordId: record?.record_id ?? "",
        scopes: askedScopes === null ? null : ticked.filter((scope) => askedScopes.includes(scope)),
      }),
  };
  const deny = useKeyedWrite<StateResponse>({
    method: "POST",
    write: (_variables, bounds) =>
      api.denyCredentialRecord(record?.record_id ?? "", { code: lookedUpCode }, bounds),
    inline: true,
    label: () => "Denial of a machine login",
    onSettled: () => submitGuard.release(),
    onSuccess: (decided) => {
      void queryClient.invalidateQueries({ queryKey: ["credential-pending"] });
      if (decided === null) lookup.mutate(lookedUpCode);
    },
  });

  useDocumentTitle("Machine login · Dispatch");

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    submitGuard.guard(() => {
      approve.reset();
      deny.reset();
      lookup.mutate(code);
    });
  };
  // What a decision made on this page left: an approval names the launcher credential it minted,
  // and a denial names none, which is the broker's own split between the two answers.
  const approved = approve.data ?? undefined;
  const denied = deny.data ?? undefined;
  const madeHere =
    approved !== undefined
      ? { credential_id: approved.credential_id, state: approved.state }
      : denied === undefined
        ? undefined
        : { credential_id: null, state: denied.state };
  // The looked-up record and its decision as the page now knows them: a decision made here leaves
  // the record decided, with the credential an approval minted.
  const view =
    record === undefined
      ? undefined
      : madeHere === undefined
        ? { decided: record.decided, record }
        : {
            decided: { credential_id: madeHere.credential_id, event: madeHere.state },
            record: { ...record, state: madeHere.state },
          };

  return (
    // The page is as wide as the Settings page (`max-w-4xl`), which holds the machine logins list's
    // six columns; at the reading width (`max-w-2xl`) the list scrolled sideways and hid Expires and
    // Revoke. The lookup's error and the looked-up login keep the reading width.
    <section className="max-w-4xl space-y-6">
      <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>Enter machine login code</h1>
      <form className="flex flex-wrap items-end gap-3" onSubmit={submit}>
        <div className="flex flex-col gap-1">
          <label className={`text-sm font-medium ${textMutedOnCanvas}`} htmlFor="machine-code">
            Code shown on the machine
          </label>
          <input
            className={`rounded-lg border px-3 py-2 font-mono ${inputClasses(false)} ${textPrimaryOnCanvas}`}
            id="machine-code"
            onChange={(event) => {
              const cleaned = event.target.value
                .toUpperCase()
                .replaceAll(/[^A-Z0-9]/g, "")
                .slice(0, 8);
              setCode(cleaned.length > 4 ? `${cleaned.slice(0, 4)}-${cleaned.slice(4)}` : cleaned);
            }}
            placeholder="XXXX-XXXX"
            value={code}
          />
        </div>
        <button
          className={`min-h-11 rounded-lg px-4 py-2 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
          disabled={lookup.isPending || code.trim() === ""}
          type="submit"
        >
          Look up
        </button>
      </form>
      {/* A lookup the reader stopped may have been made, and Look up is a press of its own, under
          a key of its own. */}
      {lookup.isError ? (
        <p className={`max-w-2xl ${dangerText}`}>
          {lookup.error instanceof SendStoppedError
            ? stoppedWriteNote("the lookup of that code", "made", {
                anew: "Look up sends the code in the field as a new lookup.",
              })
            : apiErrorMessage(lookup.error, "Could not look up that code.")}
        </p>
      ) : null}
      {view === undefined ? null : (
        <div className="max-w-2xl space-y-4">
          <CredentialRecordFacts onTickedChange={setTicked} record={view.record} ticked={ticked} />
          {view.record.state === "expired" ? (
            <p className={`text-sm font-medium ${textPrimaryOnCanvas}`}>
              This login expired before anyone decided it.
            </p>
          ) : view.decided !== null ? (
            <MachineLoginDecision
              credentialId={view.decided.credential_id}
              event={view.decided.event}
              host={view.record.identifiers?.[0] ?? ""}
              reason={view.record.reason}
              service={view.record.service}
              signIn={
                askedScopes === null
                  ? null
                  : {
                      asked: askedScopes,
                      approved: approved === undefined ? null : (approve.variables?.scopes ?? null),
                    }
              }
            />
          ) : view.record.state === "pending" ? (
            <CredentialDecisionButtons
              approve={view.record.service === null ? approval : undefined}
              deny={deny}
              submitGuard={submitGuard}
            />
          ) : null}
        </div>
      )}
      <MachineLoginsSection />
    </section>
  );
}
