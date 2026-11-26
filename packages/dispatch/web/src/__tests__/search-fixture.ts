import type {
  Agent,
  ArchitectureTree,
  IssueCollaborators,
  SearchResponse,
  SearchResult,
} from "../api/types";

/**
 * What `GET /api/v1/search` answers for `hits`: one whole page holding all of them, which is
 * what a fixture that writes out its own hits stands for - `total` and `reachable` are the hits
 * themselves, the page starts at the first, and `limit` is the route's own default (20). Name
 * `took_ms`, `degraded` or a paging field on top for a test about paging, about a degraded
 * search, or about how long the search took.
 */
export function searchResponse(
  hits: readonly SearchResult[],
  fields: Partial<SearchResponse> = {}
): SearchResponse {
  return {
    // The route echoes the four filters it narrowed by on every answer; a fixture narrows by
    // none, so it echoes none.
    filters: {},
    limit: 20,
    offset: 0,
    // Every answer names the forms the query was read as and whether the fallback is what
    // answered; a fixture's query holds no operator and broadened nothing.
    query: { broadened: false, operators: [] },
    reachable: hits.length,
    results: [...hits],
    took_ms: 1,
    total: hits.length,
    ...fields,
  };
}

/**
 * The three reads behind a box that hosts the reference picker, as a test that is about
 * something else answers them: a project with no components, an issue with no agent on it, and
 * one live session. A read no handler answers fails its test, so every such box needs all
 * three; `overrides` carries whatever the test is actually about - the person on the issue, the
 * component the picker lists - as `searchResponse`'s own does.
 */
export function architectureTree(overrides: Partial<ArchitectureTree> = {}): ArchitectureTree {
  return {
    components: [],
    not_architectural: [],
    retired_links: [],
    sources: [],
    totals: {
      components_without_work: 0,
      issues_done: 0,
      issues_total: 0,
      not_architectural: 0,
      retired_links: 0,
      unassigned: 0,
    },
    unassigned: [],
    ...overrides,
  };
}

/** CORE-1's collaborators, the registry answering and no agent on the issue. */
export function issueCollaborators(
  overrides: Partial<IssueCollaborators> = {}
): IssueCollaborators {
  return {
    agents: [],
    issue_key: "CORE-1",
    legion_registry: "ok",
    people: [],
    ...overrides,
  };
}

/** The live session every picker fixture lists: running, seen just now, holding the planner
 *  role. `last_seen` is read against the clock the test runs on, so the row reads as seen just
 *  now wherever it is drawn. */
export function plannerAgent(overrides: Partial<Agent> = {}): Agent {
  const now = Date.now();
  return {
    capabilities: ["steer"],
    dir: "/workspaces/planner",
    host: "build-host",
    last_activity: new Date(now).toISOString(),
    last_seen: now,
    machine_id: "build-host",
    open_asks: 0,
    roles: ["planner"],
    session_id: "planner-session",
    state: "live",
    title: "Planner",
    ...overrides,
  };
}
