import { createContext, type ReactNode, useContext } from "react";

import type { Agent } from "../../api/types";

type TitleSelector = (agents: Agent[]) => Agent[];
const AgentTitleSelector = createContext<TitleSelector | undefined>(undefined);

/** One selector per issue page, shared by its plain-text name readers. The selected snapshot is
 * unchanged when the registry's titles are unchanged. The registry cache still holds the full
 * answer, including the current heartbeats and states for its enabled readers. */
export function AgentTitleScope({
  children,
  select,
}: {
  readonly children: ReactNode;
  readonly select: TitleSelector;
}): ReactNode {
  return <AgentTitleSelector.Provider value={select}>{children}</AgentTitleSelector.Provider>;
}

export function useAgentTitleSelector() {
  return useContext(AgentTitleSelector);
}
