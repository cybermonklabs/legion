import type { Agent, IssueCollaboratorAgent } from "../../api/types";
import type { AgentText } from "./AgentName";
import { REF_PICKER_DEFAULT_WORDS, type RefPickerWords } from "./ref-picker";
import { recordedRoles, roleHolders } from "./role-holders";

/**
 * The parties a box can address: the live sessions, the roles their holders carry, and the
 * issue's Legion roles nobody holds now. One list, read by the shared text field's delivery
 * modes (`MentionComposer.tsx`) and by the reference picker's party rows (`useRefPicker.ts`),
 * so a session offered as a mention and the same session offered as a reference can never be
 * two different rules about who is running. The roles are the two sets that make a role known
 * (`role-holders.ts`), which the renderer links a role reference by, so a role offered here is
 * one its mention links: this module adds only each option's words.
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
  return [...roleHolders(agents).entries()]
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
 * (`recordedRoles`) that no live agent holds (`roleHolders`, the set the live role options are),
 * addressed `role:<token>` and drawn by the session `recordedRoles` shows the role by, its newest,
 * so a replaced session is never an option of its own. A role whose newest session the live list
 * names but which no longer claims the role is one of them: nobody holds it, and its mention is
 * linked and not told, as every other role nobody holds.
 *
 * The options keep the server's order, which the collaborators contract states - the registry's
 * agents by role in workflow order, then newest first - each token where the answer first names
 * it (`recordedRoles`), a token every session of which was replaced among them. Each option's
 * detail says, after its role, that nobody running holds it and so it is not told, in the words
 * the picker draws (`RefPickerWords.stoppedRole`).
 */
export function stoppedRoleOptions(
  live: readonly Agent[],
  collaborators: readonly IssueCollaboratorAgent[],
  text: Pick<AgentText, "sessionText">,
  notRunning: RefPickerWords["stoppedRole"] = REF_PICKER_DEFAULT_WORDS.stoppedRole
): MentionOption[] {
  const held = roleHolders(live);
  return [...recordedRoles(collaborators)]
    .filter(([token]) => !held.has(token))
    .map(([token, agent]) => {
      // The title the server gives is the registry's, else the session's newest stamped one; the
      // name is still `useAgentText`'s, as a live option's holder is.
      const storedTitle = agent.title ?? "";
      return {
        capabilities: [],
        // Not running, so a mention of it is refused rather than kept for its next session: the
        // line says so, as the box's hint does.
        detail: `${agent.legion.role} · ${notRunning}`,
        holder: text.sessionText(agent.session_id, storedTitle),
        sessionId: agent.session_id,
        sessionTitle: storedTitle,
        target: `role:${token}`,
        title: token,
      };
    });
}
