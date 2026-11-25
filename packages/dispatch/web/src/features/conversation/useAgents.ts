import { type RefetchOptions, useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { isAgentListRefilling } from "../../api/client";
import type { Agent } from "../../api/types";
import { agentsQuery, EMPTY_AGENTS, useRegistryPolling } from "../refs/agent-registry";

/**
 * The agent registry list. `agents` keeps the rows of the last successful read through a failed
 * refresh, so a view need not unmount what it shows. Those rows are that read's word: a failed
 * read brings no list, so it neither ends a fact read from them nor starts one. A claim that an
 * agent is gone (ended, offline, not matching, missing a capability, holding a lapsed claim)
 * starts only from a `current` list - its latest read succeeded; a view may keep a fact a current
 * list gave through a failed refresh, as the live view's header and transcript line do from the
 * kept rows. `refilling` is the failed read's reason when Envoy answered that its list is
 * refilling (`isAgentListRefilling`), which a view says in place of an unreachable listener.
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
  refetch: (options?: RefetchOptions) => Promise<unknown>;
  refilling: boolean;
} {
  useRegistryPolling(enabled && refreshWhileOpen);
  const { data, error, isError, isPending, refetch } = useQuery({
    ...agentsQuery(),
    enabled,
    // The poller refreshes what a polled caller reads; refetching it again on mount would put
    // a second request inside the poller's 15 s.
    refetchOnMount: !refreshWhileOpen,
  });
  const agents = data ?? EMPTY_AGENTS;
  const current = !isError && data !== undefined;
  const refilling = isError && isAgentListRefilling(error);

  return useMemo(
    () => ({
      agents,
      current,
      error: error instanceof Error ? error.message : isError ? "network error" : undefined,
      isError,
      isPending,
      refetch,
      refilling,
    }),
    [agents, current, error, isError, isPending, refetch, refilling]
  );
}
