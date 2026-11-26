import { queryOptions } from "@tanstack/react-query";

import { api } from "../../../api/client";

/** The machine logins the viewer may revoke that can still reach a secret (their own machines',
 *  their own pods' and every service's), as `MachineLoginsSection`
 *  lists them; its key is the one approving a login on the machine-login page and revoking one
 *  both invalidate. */
export const machineLoginsQuery = () =>
  queryOptions({
    queryKey: ["machine-logins"],
    queryFn: () => api.getMachineLogins(),
  });

/** A machine login's machine as every credentials page names it: `<host> (your pod)` for a
 *  person's pod login, `<service> on <host>` for a service's login (the Legion daemon's), and
 *  the host alone for one of the viewer's own machines.
 *
 *  A person's pod login is the login of a pod whose Kubernetes account the broker's
 *  BROKER_POD_OPERATORS maps to a person, and the broker's `person` names that person. Only that
 *  person's list holds the login, so the person is the viewer. The broker also sets its
 *  `service`, the login service the account logs in as, so `person` is read first: the row is
 *  the viewer's own and must not read as a service's login. `person` is absent from a looked-up
 *  record, which is never a pod's login, and from a broker older than the field; both read as no
 *  person. */
export function machineName({
  host,
  person,
  service,
}: {
  host: string;
  person?: string | null;
  service: string | null;
}): string {
  if (person != null) return `${host} (your pod)`;
  return service ? `${service} on ${host}` : host;
}
