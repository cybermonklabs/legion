import { queryOptions, useQuery } from "@tanstack/react-query";

import { api } from "../../../api/client";
import { useHoldsPersonScope } from "../../../hooks/useHoldsPersonScope";

/** What the credential pages may offer against this deployment's secrets broker. The server
 *  relays it from the broker's own RFC 8414 metadata behind a cache of its own, so a page asks
 *  once and holds the answer for as long as it is open: the fact changes only when the broker is
 *  deployed again, and a reload is what a person does after that. */
export const credentialCapabilitiesQuery = () =>
  queryOptions({
    queryKey: ["credential-capabilities"],
    queryFn: () => api.getCredentialCapabilities(),
    staleTime: Number.POSITIVE_INFINITY,
  });

/** The read of what the credential pages may offer (`credentialCapabilitiesQuery`), under the
 *  `person` scope: it is asked for once a read says the viewer's credential holds it
 *  (`useHoldsPersonScope`). The machine-login route and the Inbox's credential requests read the
 *  key here. */
export function useCredentialCapabilities() {
  return useQuery({ ...credentialCapabilitiesQuery(), enabled: useHoldsPersonScope() });
}

/** Whether this deployment serves the page where a person types a machine's code: only while the
 *  broker still serves the device grant. False while the answer is still loading, where it is not
 *  asked for (`useCredentialCapabilities`) and for a read that failed, so no link offers a page
 *  that is not there; the server already answers for a broker it could not reach itself, so a
 *  failure here is Dispatch's own and the page it would lead to would not load either. */
export function useMachineCodePage(): boolean {
  return useCredentialCapabilities().data?.machine_code_page ?? false;
}
