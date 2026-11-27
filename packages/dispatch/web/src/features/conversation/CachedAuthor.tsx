import type { ReactNode } from "react";
import type { Actor } from "../../api/types";
import { useAgentText } from "../refs/AgentName";

/**
 * How a part names an agent. `live`, on today's surfaces and in the Inbox, draws `AgentName`, as
 * `main` does: its session form reads the live agent registry, which starts the agents poll.
 * `cache` names the agent from the registry the cache already holds (`useAgentText`), and starts
 * no read, for a caller that sends no agents read when it draws
 * (dispatch://INFRA-5702/spec#b-r-reads).
 */
export type AgentNames = "live" | "cache";

/** An author's name from the registry the cache already holds, which starts no read: for a
 *  caller of the reply quote that sends no agents read when it draws
 *  (dispatch://INFRA-5702/spec#b-r-reads). */
export function CachedAuthor({ actor }: { actor: Actor }): ReactNode {
  return useAgentText().author(actor);
}
