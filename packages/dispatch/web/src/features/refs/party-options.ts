import type { Agent, IssueCollaboratorAgent, IssueCollaboratorLegionAgent } from "../../api/types";
import type { AgentText } from "./AgentName";

/**
 * The parties a box can address: the live sessions, the roles their holders carry, and the
 * issue's Legion roles nobody holds now. One list, read by the shared text field's delivery
 * modes (`MentionComposer.tsx`) and by the reference picker's party rows (`useRefPicker.ts`),
 * so a session offered as a mention and the same session offered as a reference can never be
 * two different rules about who is running.
 *
 * A module of its own rather than part of the field: the field hosts the picker, so the picker
 * cannot read these from the field without the two importing each other (`noImportCycles`).
 *
 * It costs no read of its own: the pair of sources is the shared registry query
 * (`agentsQuery`, which the shell polls) and the issue's collaborators, both already fetched.
 */
export interface MentionOption {
  readonly capabilities: readonly string[];
  /** Where and when: the session's directory and when it was last seen; for a role no live agent
   *  holds, the role and that it is not running. */
  readonly detail: string;
  /** The name of the session the option is, or of the one holding its role, as `useAgentText`
   *  gives it: the filter matches it, and a role option's detail tooltip names it. */
  readonly holder: string;
  /** The session the option is, or, for a role, the session holding it (the newest one for a
   *  role nobody holds): `AgentName` draws it. */
  readonly sessionId: string;
  /** That session's title as the agent list gave it: its name when the registry cache has none. */
  readonly sessionTitle: string;
  readonly target: string;
  /** The text the mention inserts (`@Title`) and the option's accessible name: a role's name, or
   *  the session's name as `useAgentText` gives it. */
  readonly title: string;
}

function roleOptions(
  agents: readonly Agent[],
  text: Pick<AgentText, "sessionText">
): MentionOption[] {
  const byRole = new Map<string, Agent>();
  for (const agent of agents) {
    for (const role of agent.roles ?? []) byRole.set(role, agent);
  }
  return [...byRole.entries()]
    .sort(([left], [right]) => {
      const leftController = left.includes("controller");
      const rightController = right.includes("controller");
      return leftController === rightController
        ? left.localeCompare(right)
        : leftController
          ? -1
          : 1;
    })
    .map(([role, agent]) => ({
      capabilities: agent.capabilities ?? [],
      detail: `${agent.dir} · ${new Date(agent.last_seen).toLocaleString()}`,
      holder: text.sessionText(agent.session_id, agent.title),
      sessionId: agent.session_id,
      sessionTitle: agent.title,
      target: `role:${role}`,
      title: role,
    }));
}

export function mentionOptions(
  agents: readonly Agent[],
  text: Pick<AgentText, "sessionText">
): MentionOption[] {
  return [
    ...roleOptions(agents, text),
    ...[...agents]
      .sort(
        (left, right) => right.last_seen - left.last_seen || left.title.localeCompare(right.title)
      )
      .map((agent) => ({
        capabilities: agent.capabilities ?? [],
        detail: `${agent.dir} · ${new Date(agent.last_seen).toLocaleString()}`,
        holder: text.sessionText(agent.session_id, agent.title),
        sessionId: agent.session_id,
        sessionTitle: agent.title,
        target: `session:${agent.session_id}`,
        title: text.sessionText(agent.session_id, agent.title),
      })),
  ];
}

/**
 * The picker's "Not running" options: one per Legion role token the issue's collaborators record
 * (`IssueCollaborators.agents[].legion`) that no live agent holds - the live agents' `roles`, the
 * rule the live role options follow - addressed `role:<token>` and drawn by the role's newest
 * session, the one entry per token the server leaves unreplaced (`legion.replaced_by` null), so a
 * replaced session is never an option of its own. A session the live list itself names is left
 * out whatever its roles say: a live session that claims no role would otherwise be offered here
 * and draw its own `Running` mark, so one row would read running and not running at once.
 *
 * The options keep the server's order, which the collaborators contract states: the registry's
 * agents by role in workflow order, then newest first.
 */
export function stoppedRoleOptions(
  live: readonly Agent[],
  collaborators: readonly IssueCollaboratorAgent[],
  text: Pick<AgentText, "sessionText">
): MentionOption[] {
  const held = new Set(live.flatMap((agent) => agent.roles ?? []));
  const running = new Set(live.map((agent) => agent.session_id));
  const offered = (agent: IssueCollaboratorAgent): agent is IssueCollaboratorLegionAgent =>
    agent.legion !== null &&
    agent.legion.replaced_by === null &&
    !held.has(agent.legion.role_token) &&
    !running.has(agent.session_id);
  return collaborators.filter(offered).map((agent) => {
    // The title the server gives is the registry's, else the session's newest stamped one; the
    // name is still `useAgentText`'s, as a live option's holder is.
    const storedTitle = agent.title ?? "";
    return {
      capabilities: [],
      detail: `${agent.legion.role} · not running`,
      holder: text.sessionText(agent.session_id, storedTitle),
      sessionId: agent.session_id,
      sessionTitle: storedTitle,
      target: `role:${agent.legion.role_token}`,
      title: agent.legion.role_token,
    };
  });
}
