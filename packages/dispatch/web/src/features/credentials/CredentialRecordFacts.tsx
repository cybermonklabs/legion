import { signInScopes } from "@legion/contracts";
import type { ReactNode } from "react";

import type { RecordResponse } from "../../api/types";
import {
  checkboxAccent,
  quoteAccentBorder,
  quoteBodyText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { ACT_AS_YOU, signInScopeChoice } from "../settings/dispatch-scope-choices";
import { machineName } from "./logic/machineLogins";

const LIFETIME_UNITS: ReadonlyArray<{ seconds: number; unit: string }> = [
  { seconds: 86400, unit: "day" },
  { seconds: 3600, unit: "hour" },
  { seconds: 60, unit: "minute" },
];

/** Formats a lifetime in whole units ("15 minutes", "2 hours") rather than a raw second count, and
 *  no lifetime as the grant's actual end: its session's. */
function formatLifetime(seconds: number | null): string {
  if (seconds === null) {
    return "Until the session ends";
  }
  for (const { seconds: unitSeconds, unit } of LIFETIME_UNITS) {
    if (seconds >= unitSeconds) {
      const count = Math.round(seconds / unitSeconds);
      return `${count} ${unit}${count === 1 ? "" : "s"}`;
    }
  }
  return `${seconds} second${seconds === 1 ? "" : "s"}`;
}

/** The approver a record names when any signed-in person may decide it: a service's machine login
 *  here, and, on the broker's own page, a shared secret's request. */
const ANYONE_APPROVER = "anyone";

/** One label and value; the value wraps inside its grid cell, so a 64-hex policy version or a long
 *  host name never runs into the fact beside it or widens a narrow page. */
function Fact({ children, label }: { children: ReactNode; label: string }): ReactNode {
  return (
    <div>
      <dt className={textMutedOnCanvas}>{label}</dt>
      <dd className={`break-words font-medium ${textPrimaryOnCanvas}`}>{children}</dd>
    </div>
  );
}

/**
 * What a pending Dispatch sign-in asks its person to decide: a box for each scope it asked for, in
 * the order the broker lists them, a known scope in its Settings words and a word this build does
 * not know as the word itself, each ticked as `ticked` says (`MachineLoginPage` starts the ticks
 * from the known scopes but `p0` (`signInScopeStartsTicked`), so `p0` and an unknown word are
 * unticked until the person ticks them); "It asks for no scope." where it asked for none; and what
 * an approval allows: where it shows boxes, whatever is ticked, the session writes in Dispatch and
 * works on Envoy as its own session within the person's projects, and Act as you adds acting as
 * them; where it asked for none, and so shows no box, only what the sign-in may do.
 */
function DispatchSignInConsent({
  asked,
  onTickedChange,
  ticked,
}: {
  asked: readonly string[];
  onTickedChange: (scopes: string[]) => void;
  ticked: readonly string[];
}): ReactNode {
  return (
    <div className="space-y-3">
      {asked.length === 0 ? (
        <p className={`text-sm ${textPrimaryOnCanvas}`}>It asks for no scope.</p>
      ) : (
        <fieldset className="space-y-2">
          <legend className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>
            What this session or tool may do
          </legend>
          {asked.map((scope) => {
            const choice = signInScopeChoice(scope);
            return (
              <div key={scope}>
                <label className={`flex items-center gap-2 text-sm ${textPrimaryOnCanvas}`}>
                  <input
                    checked={ticked.includes(scope)}
                    className={`size-4 shrink-0 rounded ${checkboxAccent}`}
                    onChange={(event) =>
                      onTickedChange(
                        event.target.checked
                          ? [...ticked, scope]
                          : ticked.filter((held) => held !== scope)
                      )
                    }
                    type="checkbox"
                  />
                  {choice?.label ?? scope}
                </label>
                <p className={`ml-6 text-xs ${textMutedOnCanvas}`}>
                  {choice?.allows ??
                    "This page does not know what this scope allows: tick it only if you do."}
                </p>
              </div>
            );
          })}
        </fieldset>
      )}
      <p className={`text-sm font-medium ${textPrimaryOnCanvas}`}>
        {asked.length === 0 ? (
          "Approving lets this session or tool write in Dispatch and work on Envoy as its own session within your projects, and it reads no secret."
        ) : (
          <>
            Whatever is ticked, approving lets this session or tool write in Dispatch and work on
            Envoy as its own session within your projects, and it reads no secret;{" "}
            {ACT_AS_YOU.label} adds acting as you.
          </>
        )}
      </p>
    </div>
  );
}

/**
 * The broker's facts about a machine login, then the agent's stated reason as plain text
 * (no markdown pipeline - a `<blockquote>` with `whitespace-pre-wrap` renders it verbatim), then,
 * for a pending one, the sentence explaining what approving it grants: a person's machine starts
 * agent sessions as them. A service (the Legion daemon) logs in with its own Kubernetes token,
 * which no person approves, so a pending service login a broker opened before that is one the
 * broker refuses to approve, and the sentence says so. `MachineLoginPage` is the one page that
 * renders this layout, above its decision controls, and the only records it holds are the machine
 * logins its code lookup returns (`POST /api/v1/credential-requests/machine-lookup`): the kind is
 * always `launcher_credential`, and the broker leaves such a record's enrollment and both of its
 * session ids empty, so neither has a fact of its own here.
 *
 * A record whose `dispatch_scopes` is a list is a Dispatch sign-in (`signInScopes`): a pending
 * one asks its person which of its scopes to approve (`DispatchSignInConsent`), where `ticked`
 * holds the boxes ticked now and `onTickedChange` takes the next set.
 */
export function CredentialRecordFacts({
  onTickedChange,
  record,
  ticked,
}: {
  onTickedChange: (scopes: string[]) => void;
  record: RecordResponse;
  ticked: readonly string[];
}): ReactNode {
  // A record with no identifier at all is one the broker sends an empty list for; the facts read
  // the same way they do for any other, with nothing where the machine's name would be.
  const identifiers = record.identifiers ?? [];
  const askedDispatchScopes = signInScopes(record);
  return (
    <div className="space-y-4">
      <dl className="grid grid-cols-2 gap-3 text-sm">
        <Fact label="Kind">
          {askedDispatchScopes === null ? "Machine login" : "Dispatch sign-in"}
        </Fact>
        <Fact label="Identifiers">{identifiers.join(", ")}</Fact>
        <Fact label="Lifetime">{formatLifetime(record.lifetime_seconds)}</Fact>
        <Fact label="Requested">
          <Timestamp at={record.requested_at} />
        </Fact>
        <Fact label="Expires">
          <Timestamp at={record.expires_at} />
        </Fact>
        <Fact label="Policy version">{record.rules_version}</Fact>
        <Fact label="Approver">
          {record.approver === ANYONE_APPROVER ? "Anyone signed in to Dispatch" : record.approver}
        </Fact>
      </dl>
      {record.reason === "" ? null : (
        <div>
          <h2 className={`text-sm font-semibold ${textMutedOnCanvas}`}>
            The agent's stated reason
          </h2>
          <blockquote
            className={`mt-1 border-l-2 pl-3 text-sm whitespace-pre-wrap ${quoteAccentBorder} ${quoteBodyText}`}
          >
            {/* The reason the agent typed, as it typed it: the reading list's
                `credential-request-reason`. */}
            {record.reason}
          </blockquote>
        </div>
      )}
      {record.state !== "pending" ? null : askedDispatchScopes === null ? (
        <p className={`text-sm font-medium ${textPrimaryOnCanvas}`}>
          {record.service
            ? `${machineName({ host: identifiers[0] ?? "", service: record.service })} is a service's login from before services logged in with their own Kubernetes token, and no one can approve it any more: deny it, or let it expire.`
            : `Approving lets ${machineName({ host: identifiers[0] ?? "", service: null })} start agent sessions as you.`}
        </p>
      ) : (
        <DispatchSignInConsent
          asked={askedDispatchScopes}
          onTickedChange={onTickedChange}
          ticked={ticked}
        />
      )}
    </div>
  );
}
