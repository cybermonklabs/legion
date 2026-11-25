import { queryOptions, useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useSyncExternalStore } from "react";

import { api, isAgentListRefilling } from "../../api/client";
import type { Agent } from "../../api/types";

/**
 * The agent registry (`GET /api/v1/agents`: the sessions the Envoy listener lists, as of
 * Dispatch's copy of its list, at most 5 s old) as every surface reads it. One query key, one
 * lookup built per response, and one poller: `AgentRegistryPoller`, mounted once in the app
 * shell, refreshes the registry every 15 s while anything registered through `useRegistryPolling`
 * is mounted: each enabled `useAgentRegistry` reader (a session's `AgentName`, a claim chip, the
 * issues table's claim filter, the search palette's send offer), each `useAgents(…, true)`
 * caller, and each reference to a session or a role a surface draws (`useReferenceData` in
 * `reference-target.ts`), so a session renamed or a role taken up while the page is open reads
 * as it is now. A reader never polls on its own: TanStack runs one interval per observer, so N
 * readers mounted at different times would fetch up to N times per 15 s. The poller keeps
 * TanStack's default of not polling a hidden tab.
 */

/** A session's running state, from the registry only. `running`: listed by the latest
 *  successful read, with the listener calling it live. `unresponsive`: listed, but past the
 *  listener's live window and inside its gone horizon - it has missed heartbeats, it has not
 *  ended, and it keeps its roles and claims (dispatch://INFRA-392). `not-running`: the latest
 *  read succeeded and does not list it. `unknown`: no successful read yet, or the latest read
 *  failed. A row from a server that names no state is `running`, which is what membership alone
 *  ever meant. */
export type AgentState = "running" | "unresponsive" | "not-running" | "unknown";

/** The poller's cadence while anything shows an agent. */
export const AGENT_REGISTRY_POLL_MS = 15_000;

declare global {
  interface Window {
    /** The browser suite's cadence for the poller, in milliseconds, set before the page loads
     *  (`e2e/agents.ts`, `pollAgentListEvery`): the rows that wait for the list to change on an open
     *  page then wait one short poll rather than a 15 s one. Unset in production. */
    __dispatchAgentRegistryPollMs?: number;
  }
}

/** The poller's cadence: `AGENT_REGISTRY_POLL_MS`, or the browser suite's own where it set a
 *  positive one (`window.__dispatchAgentRegistryPollMs`). Read at each render of the poller. */
export function agentRegistryPollMs(): number {
  const set = typeof window === "undefined" ? undefined : window.__dispatchAgentRegistryPollMs;
  return typeof set === "number" && Number.isFinite(set) && set > 0 ? set : AGENT_REGISTRY_POLL_MS;
}

/** The registry query every reader shares. */
export const agentsQuery = () =>
  queryOptions({
    queryKey: ["agents"],
    queryFn: () => api.listAgents(),
    staleTime: 10_000,
  });

interface RegistryLookup {
  readonly byId: ReadonlyMap<string, Agent>;
  readonly titles: ReadonlyMap<string, string>;
}

export const EMPTY_AGENTS: readonly Agent[] = [];
const lookups = new WeakMap<readonly Agent[], RegistryLookup>();

/** The lookup for one registry response, built once and shared by every reader of it. */
function registryLookup(agents: readonly Agent[]): RegistryLookup {
  let lookup = lookups.get(agents);
  if (lookup === undefined) {
    lookup = {
      byId: new Map(agents.map((agent) => [agent.session_id, agent])),
      titles: new Map(agents.map((agent) => [agent.session_id, agent.title])),
    };
    lookups.set(agents, lookup);
  }
  return lookup;
}

/** The agent the registry response `agents` lists under `sessionId`, through the lookup every
 *  reader of that response shares. */
export function registeredAgent(
  agents: readonly Agent[] | undefined,
  sessionId: string
): Agent | undefined {
  return agents === undefined ? undefined : registryLookup(agents).byId.get(sessionId);
}

/** The title the registry response `agents` gives `sessionId`, trimmed: undefined for a session
 *  it does not list or lists with a blank title, which names nobody. */
export function registeredTitle(
  agents: readonly Agent[] | undefined,
  sessionId: string
): string | undefined {
  const title = registeredAgent(agents, sessionId)?.title.trim();
  return title === undefined || title === "" ? undefined : title;
}

let pollingReaders = 0;
const pollingListeners = new Set<() => void>();

function notifyPollingListeners(): void {
  for (const listener of pollingListeners) {
    listener();
  }
}

function subscribePolling(listener: () => void): () => void {
  pollingListeners.add(listener);
  return () => {
    pollingListeners.delete(listener);
  };
}

function pollingWanted(): boolean {
  return pollingReaders > 0;
}

/** Keeps the shell's `AgentRegistryPoller` refreshing the registry while `active` and mounted. */
export function useRegistryPolling(active: boolean): void {
  useEffect(() => {
    if (!active) {
      return;
    }
    pollingReaders += 1;
    notifyPollingListeners();
    return () => {
      pollingReaders -= 1;
      notifyPollingListeners();
    };
  }, [active]);
}

/** The one owner of the registry's refresh cadence, mounted once in the app shell. */
export function AgentRegistryPoller(): null {
  const wanted = useSyncExternalStore(subscribePolling, pollingWanted, pollingWanted);
  const pollMs = agentRegistryPollMs();
  useQuery({
    ...agentsQuery(),
    enabled: wanted,
    refetchInterval: wanted ? pollMs : false,
    // Coming back on after a pause refetches only data at least one poll old, so a reader that
    // remounts moments after the last one unmounted does not add a request inside the cadence.
    staleTime: pollMs,
  });
  return null;
}

export interface AgentRegistry {
  /** The session's running state (`AgentState`). */
  state(sessionId: string): AgentState;
  /** The session's current registry title, when listed with a non-blank one. */
  title(sessionId: string): string | undefined;
  /** When the listener last heard the session (epoch ms), when listed. */
  lastSeen(sessionId: string): number | undefined;
  /** Every listed session's registry title, for the contracts naming functions. */
  readonly titles: ReadonlyMap<string, string>;
  /** A read of the registry has answered and the latest one succeeded: until then every state
   *  is `unknown`, so a view judging what the states decide judges nothing yet. */
  readonly known: boolean;
  /** The latest read failed: every state is `unknown`, and a view that would judge an absence
   *  says it could not check instead. */
  readonly isError: boolean;
  /** The failed read's reason was Envoy answering that its agent list is refilling
   *  (`isAgentListRefilling`), which a view says in place of an unreachable listener. */
  readonly refilling: boolean;
}

/**
 * Reads the registry. `enabled` fetches it (a first load only: it never refetches on mount, the
 * shell's poller refreshes it) and registers with the poller; disabled, it still reads whatever
 * the cache holds, so its titles name agents but its states stay `unknown` until a read lands.
 * A read that failed is not asked again by a reader that mounts after it either: a page draws an
 * agent's name in many places, and each of them would ask a failed registry again with every
 * try the dashboard's retry rule allows, where the poller's own interval already asks it again
 * every 15 s.
 */
export function useAgentRegistry(enabled: boolean): AgentRegistry {
  useRegistryPolling(enabled);
  const { data, error, isError, isPending } = useQuery({
    ...agentsQuery(),
    enabled,
    refetchOnMount: false,
    retryOnMount: false,
  });
  const agents = data ?? EMPTY_AGENTS;
  const lookup = registryLookup(agents);
  // A failed refetch keeps the last response's data, so the state goes back to unknown while
  // the cached titles go on naming agents.
  const known = !(isPending || isError);
  const refilling = isError && isAgentListRefilling(error);
  return useMemo(
    () => ({
      isError,
      known,
      lastSeen: (sessionId: string) => lookup.byId.get(sessionId)?.last_seen,
      refilling,
      // A listed row's own `state` decides: the listener judged it from the session's last
      // registration, which is the one thing a list's membership cannot say, since the list
      // carries the unresponsive sessions beside the live ones.
      state: (sessionId: string): AgentState => {
        if (!known) return "unknown";
        const agent = lookup.byId.get(sessionId);
        if (agent === undefined) return "not-running";
        return agent.state === "unresponsive" ? "unresponsive" : "running";
      },
      title: (sessionId: string) => registeredTitle(agents, sessionId),
      titles: lookup.titles,
    }),
    [agents, isError, known, lookup, refilling]
  );
}
