import { createContext } from "react";

import type { Agent, IssueCollaboratorAgent, IssueCollaboratorLegionAgent } from "../../api/types";

/**
 * What makes a role known, defined once: a role is known while a session the agent registry
 * lists holds it (`roleHolders`), or while the collaborators of the issue the record is on record
 * it (`recordedRoles`), which are that issue's Legion roles, running or not - a role whose
 * session the registry still lists but which no longer claims it among them. The picker offers
 * exactly these roles and adds only each one's words (`party-options.ts`): a held role among the
 * live sessions' roles, and every recorded one nobody holds under "Not running". The renderer
 * names and links a role reference on the same two sets (`reference-target.ts`), so a role the
 * picker offers is a role its mention links, and no other. The registry (`GET /api/v1/agents`)
 * lists the live sessions and the unresponsive ones, each with the roles it holds. The
 * collaborators (`GET /api/v1/issues/{key}/collaborators`) list every Legion role the issue ever
 * had a session for (`legion.role_token`), one entry per session, the newest of a role's
 * sessions the one no later session replaced (`legion.replaced_by` null).
 *
 * Delivery follows the first half alone. A comment's role mention goes to the role's holder,
 * live or unresponsive, and fails at once where the role has none: the Go API's
 * `resolveDeliveryTarget`, through the Envoy client's `Role`. A recorded role nobody holds is
 * linked and not told, as the writing box's hint says.
 *
 * It is a module of its own rather than part of the picker's options, because the renderer reads
 * it too and the options module is the picker's.
 */

const holdersOf = new WeakMap<readonly Agent[], ReadonlyMap<string, Agent>>();

/** Every role the registry response `agents` lists a holder for, with that holder: built once per
 *  response and shared by every reader of it. */
export function roleHolders(agents: readonly Agent[]): ReadonlyMap<string, Agent> {
  const known = holdersOf.get(agents);
  if (known !== undefined) return known;
  const holders = new Map<string, Agent>();
  for (const agent of agents) {
    for (const role of agent.roles ?? []) holders.set(role, agent);
  }
  holdersOf.set(agents, holders);
  return holders;
}

const recordedOf = new WeakMap<
  readonly IssueCollaboratorAgent[],
  ReadonlyMap<string, IssueCollaboratorLegionAgent>
>();

/** No role recorded, for collaborators not read yet or an answer that lists no agent. */
const NO_ROLES: ReadonlyMap<string, IssueCollaboratorLegionAgent> = new Map();

/** Whether a collaborator is a Legion session, which records a role token. */
function isLegionAgent(agent: IssueCollaboratorAgent): agent is IssueCollaboratorLegionAgent {
  return agent.legion !== null;
}

/** Every Legion role token an issue's collaborators `agents` record, running or not, with the
 *  entry a reader is shown it by: the role's newest session, the entry no later one replaced,
 *  else - for a token every entry of which the answer says was replaced - its first entry. Each
 *  token stands where the answer first names it, so the tokens keep the answer's order, a token
 *  whose every entry was replaced among them. Built in one pass, once per response, and shared by
 *  every reader of it. */
export function recordedRoles(
  agents: readonly IssueCollaboratorAgent[] | null | undefined
): ReadonlyMap<string, IssueCollaboratorLegionAgent> {
  if (agents === null || agents === undefined) return NO_ROLES;
  const known = recordedOf.get(agents);
  if (known !== undefined) return known;
  const roles = new Map<string, IssueCollaboratorLegionAgent>();
  for (const agent of agents) {
    if (!isLegionAgent(agent)) continue;
    const shown = roles.get(agent.legion.role_token);
    // A key set again keeps its place in the map, so a later unreplaced entry takes over from a
    // replaced one without moving the token.
    if (
      shown === undefined ||
      (shown.legion.replaced_by !== null && agent.legion.replaced_by === null)
    ) {
      roles.set(agent.legion.role_token, agent);
    }
  }
  recordedOf.set(agents, roles);
  return roles;
}

/** The issue the record is on, where the text drawn inside is a record's (`TextSource.issue`),
 *  and undefined for a record on no issue or a text that is no record's. Each host of a record's
 *  text provides it to the references it draws: a rendered body, a one-line preview, the
 *  plain-text projection and the document editor. A reference then reads the collaborators of
 *  that issue and no other, and a host nested in another provides its own record's issue. */
export const RecordIssue = createContext<string | undefined>(undefined);
