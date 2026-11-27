import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import { invalidateWaitingForPaused } from "../../api/query-client";
import type { LauncherCredentialResp } from "../../api/types";
import { textSecondaryOnSurface } from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { credentialGrantsQuery } from "./logic/grants";
import { machineLoginsQuery, machineName } from "./logic/machineLogins";
import { type RevocableColumn, RevocableList } from "./RevocableList";

/** Who approved a login, as its "Approved by" cell says: the approver of a login a person
 *  approved, and no one for a token login. A person's pod login (`person` set) says that the
 *  broker's BROKER_POD_OPERATORS maps its Kubernetes account to the viewer, and a service's token
 *  login names the Kubernetes account that logged in. `== null`: a broker older than this
 *  Dispatch sends no `subject` and no `person` at all. */
function approvedBy(login: LauncherCredentialResp): string {
  if (login.subject == null) return login.approved_by;
  if (login.person != null) return `No one: BROKER_POD_OPERATORS maps ${login.subject} to you`;
  return `No one: ${login.subject}`;
}

/** What revoking a login ends, and what logs in again after it, as its Revoke asks before it
 *  sends: a person's pod login, one of the viewer's machines, a service's login a person
 *  approved, and a service's token login. */
function revokeOutcome(login: LauncherCredentialResp): string {
  if (login.person != null) {
    return "Every session it started ends at once. The pod logs in again with its token, under a new login, while the broker's BROKER_SERVICE_LOGINS names its Kubernetes account, and that login is yours while BROKER_POD_OPERATORS maps the account to you.";
  }
  if (!login.service) {
    return "Every agent session it started loses its secrets at once, and the machine needs a new login.";
  }
  if (login.subject == null) {
    return "Every session it started, its worker pods included, ends at once.";
  }
  return `Every session it started, its worker pods included, ends at once, and a running pod stays without secrets until it is replaced; ${login.service} logs in again with its Kubernetes token, for the pods it launches next, unless ${login.subject} is removed from the broker's BROKER_SERVICE_LOGINS.`;
}

const columns: readonly RevocableColumn<LauncherCredentialResp>[] = [
  {
    cell: (login) => (
      <>
        {machineName(login)}
        {login.expired ? (
          <span className={`block text-xs font-normal ${textSecondaryOnSurface}`}>
            expired, sessions still running
          </span>
        ) : null}
      </>
    ),
    header: "Machine",
  },
  {
    cell: (login) => <span className="font-mono text-xs">{login.credential_id}</span>,
    header: "Credential",
  },
  { cell: approvedBy, header: "Approved by" },
  { cell: (login) => <Timestamp at={login.issued_at} />, header: "Issued" },
  { cell: (login) => <Timestamp at={login.expires_at} />, header: "Expires" },
];

/**
 * The machine logins the viewer may revoke that can still reach a secret. There is one row per
 * machine logged in as them, one per pod logged in as them, and one per service's login, whoever
 * approved it. A pod logs in as the viewer when the broker's BROKER_POD_OPERATORS maps its
 * Kubernetes account to them: the broker names the viewer as the row's `person`, lists the row
 * to them alone and lets them alone revoke it, and the row reads `<host> (your pod)`. A service's
 * login (the Legion daemon's, labelled `legion-daemon on <host>`) is listed to everyone signed in,
 * and anyone signed in revokes it. Each row says which credential it is (the broker's
 * `credential_id`, which every enrollment the login made names, while its machine name is the
 * machine's own claim), who approved it, when it was issued and when it expires. No one approves
 * a token login: a pod's row says that BROKER_POD_OPERATORS maps its account to the viewer, and a
 * service's row names the Kubernetes ServiceAccount that logged in. A login's sessions outlive its
 * expiry, since each renews with its own key, so an expired login stays listed, marked `expired,
 * sessions still running`, until its last session ends.
 * Revoke ends a login, expired or not: it enrolls no more sessions, and every session it enrolled
 * (a service's worker pods, or the viewer's pod) loses the broker at once, its grants with it, so
 * Live grants is refreshed too. A pod or a service logging in with its token logs in again under
 * a new login: a pod while the broker's BROKER_SERVICE_LOGINS names its account, that login being
 * the viewer's while BROKER_POD_OPERATORS maps the account to them, and a service for
 * the pods it launches next, while a pod it launched before stays without secrets until it is
 * replaced. Dispatch names the viewer as the person revoking, and the broker refuses anyone but
 * the approver of a person's machine login and anyone but the person of a person's pod login.
 * Rendered on the machine-login page.
 */
export function MachineLoginsSection(): ReactNode {
  const queryClient = useQueryClient();
  const logins = useQuery({ ...machineLoginsQuery(), select: (data) => data.credentials ?? [] });

  return (
    <RevocableList
      className="pt-4"
      columns={columns}
      // A login's subject is the Kubernetes identity its token names, not a writer's text.
      confirm={(login) =>
        `Revoke the machine login for ${machineName(login)}? ${revokeOutcome(login)}`
      }
      description="Every machine and every pod logged in as you, and every service's login. A pod logs in as you when the broker's BROKER_POD_OPERATORS maps the pod's Kubernetes account to you. The list shows a login while it is unexpired or a session it started still runs: a session outlives its login's expiry. Only you may revoke the logins of your machines and your pods. Anyone signed in may revoke a service's login. Revoking a login ends it now: it starts no more sessions, and every session it started loses its secrets."
      empty="No live machine logins."
      heading="Machine logins"
      headingId="machine-logins-heading"
      noun="machine logins"
      onRevoked={() =>
        Promise.all([
          invalidateWaitingForPaused(queryClient, machineLoginsQuery().queryKey),
          invalidateWaitingForPaused(queryClient, credentialGrantsQuery().queryKey),
        ])
      }
      query={logins}
      readKey={machineLoginsQuery().queryKey}
      revoke={(login, bounds) => api.revokeMachineLogin(login.credential_id, bounds)}
      rowKey={(login) => login.credential_id}
      rowName={(login) => `the machine login for ${machineName(login)}`}
    />
  );
}
