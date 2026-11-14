import { useMemo } from "react";
import type { Agent, InboxRow } from "../../api/types";
import {
  type AddressSpec,
  cappedTextParam,
  commaListParam,
  useAddressFields,
} from "../../lib/page-address";
import { AGENTS_MACHINE_PARAM } from "../refs/routes";
import { AGENT_GROUPS, type AgentGroup } from "./agent-groups";

/**
 * What narrows the agent list: any number of groups, machines and roles, and a free-text
 * search. An empty list matches every session, as an empty search box does, and a list of two
 * keeps the rows of either (INFRA-812, R7: each of the three is a multi-select).
 */
export interface AgentFilters {
  readonly groups: readonly AgentGroup[];
  readonly machines: readonly string[];
  readonly roles: readonly string[];
  readonly search: string;
}

/** The page's whole narrowing as the address spells it: the free-text query (`?q=`), and the
 *  groups, machines and roles each as one comma-separated parameter (`?group=`, `?machine=`,
 *  `?role=`). The names are the parameters': `machine` is the one the machines page links in
 *  with (`AGENTS_MACHINE_PARAM`), and the other three are this page's own. Every one of them is
 *  absent while nothing is picked, so a plain `/agents` is the page as it opens and unticking
 *  the last value does not bring the filter back on reload. A module-level constant, since its
 *  identity is what `useAddressFields` memoizes on. */
const AGENT_FILTER_SPEC = {
  group: commaListParam(),
  [AGENTS_MACHINE_PARAM]: commaListParam(),
  q: cappedTextParam(),
  role: commaListParam(),
} satisfies AddressSpec;

/** One frozen empty list, so an address naming no value of a filter gives the page the same
 *  value every render rather than new filter state. */
const NONE: readonly string[] = [];

/**
 * The page's whole narrowing, read from the address and written back to it (R5: this page's
 * narrowing goes through `lib/page-address.ts` alone). All four fields live here rather than in
 * the page, so the control around them can be rebuilt without moving them.
 *
 * The machines are the address's own `?machine=` rather than part of `?q=`: the free-text query
 * matches the directory as well as the machine, so `?q=<host>` would also find a session whose
 * directory holds the machine's name, while `/machines/<machine>`'s link to
 * `/agents?machine=<host>` must name exactly the sessions on that machine. Each control is a
 * multi-select, so its parameter carries the picked values as a comma-separated list:
 * `?machine=relay-1` is the one machine that link has always meant, and two picked machines
 * read `?machine=relay-1,relay-2`.
 *
 * A group the address names that this build does not know is dropped, and the groups are read
 * back in the one order the page draws them in, whatever order they were ticked in.
 */
export function useAgentFilters(): readonly [AgentFilters, (next: AgentFilters) => void] {
  const [values, set] = useAddressFields(AGENT_FILTER_SPEC);
  const filters = useMemo<AgentFilters>(
    () => ({
      groups: AGENT_GROUPS.filter((group) => values.group.includes(group)),
      machines: values.machine.length === 0 ? NONE : values.machine,
      roles: values.role.length === 0 ? NONE : values.role,
      search: values.q,
    }),
    [values]
  );
  const setFilters = (next: AgentFilters) =>
    set({ group: next.groups, machine: next.machines, q: next.search, role: next.roles });
  return [filters, setFilters] as const;
}

/** Every word `search` names, lowercased; an empty query has none. The page splits this once per
 *  render (`AgentsPage`'s `words`) rather than once per agent, since every row's `matchesFilters`
 *  call would otherwise re-split the one query it shares. */
export function searchWords(search: string): readonly string[] {
  return search.trim().toLowerCase().split(/\s+/).filter(Boolean);
}

/** The text a session's search words match against: its title, directory, machine, session id,
 *  and the key of every issue one of its OPEN asks names - once, lowercased, so a session with
 *  no open-ask issue still matches on the other four. An issue the session answered or that
 *  closed since drops out of this list with the ask (the inbox read this comes from is open asks
 *  only), so a search by that issue's key stops finding the session. */
function searchHaystack(agent: Agent, issueKeys: readonly string[]): string {
  return [agent.title, agent.dir, agent.host, agent.session_id, ...issueKeys]
    .join(" ")
    .toLowerCase();
}

/** `words` is `searchWords(filters.search)`, split once by the caller rather than once per agent
 *  (the page's `matches` closure runs this for every row). `group` is the row's own state
 *  (`agent-groups.ts`), which the page has already worked out to place the row. `issueKeys` is
 *  every issue key an open ask by this session names (`sessionIssueKeys`, below), the free-text
 *  search's only field beyond what `Agent` itself carries. */
export function matchesFilters(
  agent: Agent,
  group: AgentGroup,
  filters: Pick<AgentFilters, "groups" | "machines" | "roles">,
  words: readonly string[],
  issueKeys: readonly string[]
): boolean {
  if (filters.groups.length > 0 && !filters.groups.includes(group)) return false;
  if (filters.machines.length > 0 && !filters.machines.includes(agent.host)) return false;
  if (
    filters.roles.length > 0 &&
    !filters.roles.some((role) => (agent.roles ?? []).includes(role))
  ) {
    return false;
  }
  if (words.length === 0) return true;
  const haystack = searchHaystack(agent, issueKeys);
  return words.every((word) => haystack.includes(word));
}

/**
 * The machines and roles the filter control offers: the ones the live sessions occupy, and every
 * machine the filter already names wherever no live session reports it.
 *
 * That last one is the whole of this rule. `?machine=` can name a host nothing live reports —
 * an old link, a machine whose sessions have ended, or `/machines/<machine>`'s own "Agents on
 * this machine" link, which that page shows even when it draws nothing. The list is then
 * honestly empty, which is the filter doing its job; but with the live hosts alone for options
 * the control would offer no row for the picked machine, read as though nothing were picked
 * while every row was hidden, and leave the reader nothing to untick — the one control that
 * seems to show the filter could not clear it. Offering the picked machines keeps the control
 * showing the filter in force, so a stale option can never hide every agent with no way back.
 */
export function filterOptions(
  agents: readonly Agent[],
  machines: readonly string[]
): {
  machines: string[];
  roles: string[];
} {
  const hosts = new Set<string>(machines.filter((machine) => machine !== ""));
  const roles = new Set<string>();
  for (const agent of agents) {
    if (agent.host !== "") hosts.add(agent.host);
    for (const role of agent.roles ?? []) roles.add(role);
  }
  return { machines: [...hosts].sort(), roles: [...roles].sort() };
}

/** Every issue key an OPEN ask by each session names, keyed by session id - the search box's
 *  "issue key" field. `rows` is the inbox read the page already fetches for its `needsYou`
 *  counts (`GET /api/v1/inbox`, open asks only: `inbox.go`'s `where a.state = 'open'`), so this
 *  costs no network call of its own, and a session's answered or resolved asks name no issue
 *  here. */
export function sessionIssueKeys(
  rows: readonly InboxRow[]
): ReadonlyMap<string, readonly string[]> {
  const keys = new Map<string, Set<string>>();
  for (const ask of rows) {
    if (ask.author.kind !== "session" || ask.issue_key === null) continue;
    const forSession = keys.get(ask.author.id) ?? new Set<string>();
    forSession.add(ask.issue_key);
    keys.set(ask.author.id, forSession);
  }
  return new Map([...keys].map(([session, forSession]) => [session, [...forSession]]));
}
