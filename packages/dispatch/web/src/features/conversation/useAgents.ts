import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { isAgentListRefilling } from "../../api/client";
import type { Agent } from "../../api/types";
import {
  agentsQuery,
  EMPTY_AGENTS,
  useAgentListFailure,
  useRegistryPolling,
} from "../refs/agent-registry";

/**
 * The agent registry list. `agents` keeps the rows of the last successful read through a failed
 * refresh, so a view need not unmount what it shows. Those rows are that read's word: a failed
 * read brings no list, so it neither ends a fact read from them nor starts one. A claim that an
 * agent is gone (ended, offline, not matching, missing a capability, holding a lapsed claim)
 * starts only from a `current` list - its latest read succeeded; a view may keep a fact a current
 * list gave through a failed refresh, as the live view's header and transcript line do from the
 * kept rows. `refilling` is the failed read's reason when Envoy answered that its list is
 * refilling (`isAgentListRefilling`), which a view says in place of an unreachable listener.
 * Each field says what the list's reads last settled, as the registry's do (`useAgentRegistry`):
 * `isPending` holds only until a first read settles, and a failure - `isError`, `error` and
 * `refilling` - stands through every later read until one succeeds.
 * `refreshWhileOpen` keeps the list fresh through the shell's one poller (`AgentRegistryPoller`)
 * while this caller is mounted and enabled, instead of an interval of its own.
 */
export function useAgents(
  enabled: boolean,
  refreshWhileOpen = false
): {
  agents: readonly Agent[];
  current: boolean;
  error: string | undefined;
  isError: boolean;
  isPending: boolean;
  refetch: () => Promise<unknown>;
  refilling: boolean;
} {
  useRegistryPolling(enabled && refreshWhileOpen);
  const { data, error, errorUpdateCount, refetch, status } = useQuery({
    ...agentsQuery(),
    enabled,
    // The poller refreshes what a polled caller reads; refetching it again on mount would put
    // a second request inside the poller's 15 s.
    refetchOnMount: !refreshWhileOpen,
  });
  const agents = data ?? EMPTY_AGENTS;
  // What the reads last settled, rather than what a read is doing now: a list that has only ever
  // failed goes back to pending, with no error, while each later read is out, and a view that read
  // that as no answer yet would put its loading state in place of the failure it said, and say the
  // failure again when the read lands on it. A failure counts once it is recorded
  // (`errorUpdateCount`) and stands until a read succeeds, with its reason (`useAgentListFailure`).
  const current = status === "success";
  const isError = !current && errorUpdateCount > 0;
  const failure = useAgentListFailure(error);
  const refilling = isError && isAgentListRefilling(failure);

  return useMemo(
    () => ({
      agents,
      current,
      error: isError ? (failure instanceof Error ? failure.message : "network error") : undefined,
      isError,
      isPending: !current && !isError,
      refetch,
      refilling,
    }),
    [agents, current, failure, isError, refetch, refilling]
  );
}
