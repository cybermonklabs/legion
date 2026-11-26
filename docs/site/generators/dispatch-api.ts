#!/usr/bin/env bun
// Writes <content dir>/dispatch/reference/api.md and api-v1.md from Dispatch's route table
// (packages/envoy/internal/dispatch/api/routes_table.go), read through `envoy-dispatch routes
// --version 2` and `envoy-dispatch routes`, which print the bodies GET /api/v2 and GET /api/v1
// serve without a database or a listener. api.md is /api/v2, the API to call; api-v1.md is the
// deprecated /api/v1, kept while clients still call it. Contract: scripts/generate.ts;
// lib/reference.ts runs the binary.
import type { DispatchScope } from "../../../packages/contracts/src/dispatch-api-core.ts";
import { inline, writePage } from "./lib/markdown.ts";
import { readTable } from "./lib/reference.ts";

/** The release that first serves /api/v2. No build knows the version of its own release (the
 *  Envoy image is built before the release job picks its `legion-envoy` version), so the pages
 *  name it by the pull request it carries. */
const V2_FIRST_RELEASE = "the first `legion-envoy` release that carries pull request #4274";

/** Who may call a route, by the table's auth value (`routeAuth` in the source). */
const CALLERS: Record<string, { label: string; meaning: string }> = {
  public: { label: "Anyone", meaning: "no credential is needed." },
  any: {
    label: "Human or agent",
    meaning: "a signed-in human (browser session) or an agent (bearer token).",
  },
  bearer: {
    label: "Agent only",
    meaning: "an agent's bearer token; a browser session is refused.",
  },
};

/** What each OAuth scope a Dispatch credential can hold lets it do (`routeScopes` in the source,
 *  `model.ScopePerson`, `model.ScopeApprove` and `model.ScopeP0`). `satisfies` ties the entries to
 *  `DISPATCH_SCOPES`: a scope added there with no entry here fails the typecheck. */
const SCOPES = {
  person:
    "this credential acts as its person. A browser sign-in holds it, so does a personal token unless its owner took it away, and so does a token the secrets broker issued that acts for its person.",
  approve:
    "beside `person`, this credential may do what settles work or controls a credential: a document review, keeping a decision, the answer of an ask of kind `approval`, a machine-login decision, a credential-grant, machine-login or sign-in revoke, and minting, changing or revoking a personal token. A browser sign-in holds it, and so does a personal token its owner gave it; of the tokens the secrets broker issues, only the approve-only token holds it, which no machine holds and which calls only the approve list's routes; and no credential holds `approve` without `person`.",
  p0: "opens no route. A priority of 0 on `POST /api/v2/issues` and `PATCH /api/v2/issues/{key}`, and on the same two routes under `/api/v1`, needs it when the write makes the issue's own priority 0. Keeping P0, lowering it, clearing it and priorities 1 to 3 need no scope. A browser sign-in holds `p0`. A personal token holds it only where its maker gave it, and may hold it alone: no mint gives it by default. No broker token, shared token or service-account token holds `p0`.",
} satisfies Record<DispatchScope, string>;

/** What a /api/v2 route serves, by its index entry's `kind` (`resourceKind` in the source,
 *  routes_versions.go). A route of the delivery pages, which keeps its /api/v1 shape, names none. */
const KINDS: Record<string, { label: string; meaning: string }> = {
  collection: {
    label: "a list",
    meaning:
      "a list of items. It answers `{items, total, limit, offset}` and takes `limit` and `offset`, and a `POST` to it creates an item.",
  },
  log: {
    label: "a log",
    meaning:
      "a list read in order by one position field, which its `after` and `before` take, answered as a list is.",
  },
  item: { label: "an item", meaning: "one item, at its list's path and its key." },
  singleton: {
    label: "a singleton",
    meaning:
      "one object with no key of its own: a setting, a project's own record, an issue's claim, the caller.",
  },
  state: {
    label: "a state",
    meaning: "an item's state, which a `PUT` moves with a body whose `state` names the new one.",
  },
  report: {
    label: "a report",
    meaning:
      "a read computed from what Dispatch stores that is no list, so it answers no envelope.",
  },
  computation: { label: "a computation", meaning: "a computation that stores nothing." },
  bytes: { label: "bytes", meaning: "a read that answers bytes: a file, or a document's version." },
  stream: { label: "a stream", meaning: "a read that answers server-sent events." },
};

/** How a log's position field is written in `after` and `before` (`positionType`). */
const POSITIONS: Record<string, string> = {
  integer: "an integer",
  "time,id": "a time and an id, `<RFC 3339 time>,<id>`",
};

interface Route {
  method: string;
  path: string;
  auth: string;
  scopes?: string[];
  description: string;
}

/** A route as GET /api/v2 lists it: every member present, the declarations a client reads to
 *  call it beside the five a /api/v1 route has (`routeIndexV2Entry` in the source). */
interface RouteV2 extends Route {
  scopes: string[];
  resource: string;
  kind: string;
  key: string;
  time_fields: string[];
  position: { field: string; type: string } | null;
  list: { default_limit: number | null; max_limit: number | null } | null;
  cross_project: boolean;
}

/** Refuses a route whose auth or scopes this page has no words for, so a new value fails the
 *  build rather than rendering blank. */
function checkCaller(route: Record<string, unknown>, scopes: unknown): void {
  if (!Object.hasOwn(CALLERS, route.auth as string)) {
    throw new Error(`route ${route.method} ${route.path} has unknown auth ${route.auth}`);
  }
  if (!Array.isArray(scopes)) {
    throw new Error(`route ${route.method} ${route.path} has unreadable scopes`);
  }
  for (const scope of scopes) {
    if (typeof scope !== "string" || !Object.hasOwn(SCOPES, scope)) {
      throw new Error(`route ${route.method} ${route.path} needs unknown scope ${scope}`);
    }
  }
}

function readRoutesV1(): Route[] {
  return readTable("envoy-dispatch", ["routes"], "routes", [
    "method",
    "path",
    "auth",
    "description",
  ]).map((route) => {
    // A /api/v1 route that needs no scope leaves the field out, so a present one names one or more.
    if (route.scopes !== undefined && (!Array.isArray(route.scopes) || route.scopes.length === 0)) {
      throw new Error(`route ${route.method} ${route.path} has unreadable scopes`);
    }
    checkCaller(route, route.scopes ?? []);
    return route as unknown as Route;
  });
}

function readRoutesV2(): RouteV2[] {
  return readTable("envoy-dispatch", ["routes", "--version", "2"], "items", [
    "method",
    "path",
    "auth",
    "description",
  ]).map((route) => {
    const where = `route ${route.method} ${route.path}`;
    // GET /api/v2 writes every member of every route, empty where the route declares nothing.
    checkCaller(route, route.scopes);
    for (const field of ["resource", "kind", "key"]) {
      if (typeof route[field] !== "string") {
        throw new Error(`${where} has a ${field} that is not text`);
      }
    }
    if (route.kind !== "" && !Object.hasOwn(KINDS, route.kind as string)) {
      throw new Error(`${where} has unknown kind ${route.kind}`);
    }
    const timeFields = route.time_fields;
    if (!Array.isArray(timeFields) || timeFields.some((field) => typeof field !== "string")) {
      throw new Error(`${where} has unreadable time_fields`);
    }
    const position = route.position as Record<string, unknown> | null;
    if (
      position !== null &&
      (typeof position?.field !== "string" || !Object.hasOwn(POSITIONS, position.type as string))
    ) {
      throw new Error(`${where} has unreadable position ${JSON.stringify(position)}`);
    }
    const list = route.list as Record<string, unknown> | null;
    if (list !== null) {
      const bounds = [list?.default_limit, list?.max_limit];
      const whole = bounds.every((bound) => bound === null);
      if (!whole && !bounds.every((bound) => typeof bound === "number")) {
        throw new Error(`${where} has unreadable list ${JSON.stringify(list)}`);
      }
    }
    if (typeof route.cross_project !== "boolean") {
      throw new Error(`${where} has a cross_project that is not true or false`);
    }
    return route as unknown as RouteV2;
  });
}

/** Names as a sentence lists them, each a code span: `` `person` `` or `` `person` and `approve` ``. */
function codeList(names: readonly string[]): string {
  const spans = names.map((name) => `\`${name}\``);
  return spans.length === 1 ? spans[0] : `${spans.slice(0, -1).join(", ")} and ${spans.at(-1)}`;
}

/** The sections both pages open with: who may call a route and the scopes it can need. */
function callerSections(): string[] {
  return [
    "## Who may call a route",
    "",
    ...Object.values(CALLERS).map(({ label, meaning }) => `- **${label}**: ${meaning}`),
    "",
    "## The scopes a route or field value needs",
    "",
    'A route, or one value of a field on a route, can need OAuth scopes of the credential that calls it. The credential must hold every scope the route or value needs. A credential that lacks a needed scope is refused `403 OAUTH_SCOPE_REQUIRED` naming the scope, under `WWW-Authenticate: Bearer error="insufficient_scope", scope="<scope>"` (RFC 6750 section 3.1). The shared agent token a deployment configures, a Kubernetes service-account token and a broker-issued token scoped to a project hold no OAuth scope.',
    "",
    ...Object.entries(SCOPES).map(([scope, meaning]) => `- \`${scope}\`: ${meaning}`),
    "",
    "A read-only token is any token that holds `read`: one the secrets broker minted from a read entry or narrowed because its trade asked for `read`, or a personal token minted with `read`. It holds `person` at most, never `approve` or `p0`. Dispatch serves it only on the GET routes it classes as reads (`readClasses` in the source), and refuses it `403 READ_ONLY_TOKEN` on every other route, with no challenge header, before it reads the route's scopes. It refuses a read-only token that carries a cut `403 AS_OF_TOKEN_NOT_SERVED` on every read but `GET /api/v1/whoami` and `GET /api/v2/me`. The route index does not carry the read class, so this page does not mark those routes.",
    "",
    "Path parameters are the `{name}` segments of a path.",
    "",
  ];
}

/** One route's heading, description and the lines every route has: who may call it, its scopes
 *  and its path parameters. */
function routeLines(route: Route): string[] {
  const lines = [`### ${route.method} ${route.path}`, "", inline(route.description), ""];
  lines.push(`- **Who may call:** ${CALLERS[route.auth]?.label}`);
  if (route.scopes !== undefined && route.scopes.length > 0) {
    lines.push(`- **Needs the scope:** ${codeList(route.scopes)}`);
  }
  const parameters = [...route.path.matchAll(/\{([^{}]+)\}/g)].map((match) => `\`${match[1]}\``);
  if (parameters.length > 0) {
    lines.push(`- **Path parameters:** ${parameters.join(", ")}`);
  }
  return lines;
}

/** The lines a /api/v2 route adds: what it serves and how a client reads it. */
function routeLinesV2(route: RouteV2): string[] {
  const lines: string[] = [];
  if (route.resource !== "" && route.kind !== "") {
    const keyed = route.key === "" ? "" : `, keyed by \`${route.key}\``;
    lines.push(`- **Serves:** \`${route.resource}\`, ${KINDS[route.kind]?.label}${keyed}`);
  }
  if (route.time_fields.length === 1) {
    lines.push(`- **Time window:** \`from\` and \`to\` on \`${route.time_fields[0]}\``);
  } else if (route.time_fields.length > 1) {
    const fields = route.time_fields.map((field) => `\`${field}_from\` and \`${field}_to\``);
    lines.push(`- **Time windows:** ${fields.join("; ")}`);
  }
  if (route.position !== null) {
    lines.push(
      `- **Read in order by:** \`${route.position.field}\`, ${POSITIONS[route.position.type]}, in \`after\` and \`before\``
    );
  }
  if (route.list !== null) {
    lines.push(
      route.list.max_limit === null
        ? "- **Page:** every item, unless the request names a `limit`"
        : `- **Page:** ${route.list.default_limit} items when the request names no \`limit\`, at most ${route.list.max_limit}`
    );
  }
  if (route.cross_project) {
    lines.push(
      "- **Across projects:** every project unless a repeatable `project` filter names some"
    );
  }
  return lines;
}

/** The routes of one version under a heading per resource: the path's segment after the version
 *  prefix (/api/v2/issues/{key}/asks is an issues route), and the index itself is "". */
function groupedRoutes<R extends Route>(routes: R[], extra: (route: R) => string[]): string[] {
  const groups = Map.groupBy(routes, (route) => route.path.split("/")[3] ?? "");
  const lines: string[] = [];
  for (const resource of [...groups.keys()].sort()) {
    const words = resource.replaceAll("-", " ");
    const title = resource === "" ? "Index" : words.charAt(0).toUpperCase() + words.slice(1);
    lines.push(`## ${title}`, "");
    for (const route of groups.get(resource) ?? []) {
      lines.push(...routeLines(route), ...extra(route), "");
    }
  }
  return lines;
}

/** The paths of the routes that declare no kind: the rows another tree owns, which keep their
 *  /api/v1 shape at the same path under /api/v2 (`v2Infra979Rows` in the source). */
function unboundPaths(routes: RouteV2[]): string {
  const paths = new Set(routes.filter((route) => route.kind === "").map((route) => route.path));
  return paths.size === 0 ? "" : codeList([...paths]);
}

function renderV2(routes: RouteV2[]): string {
  const unbound = unboundPaths(routes);
  return [
    `Dispatch serves ${routes.length} routes under \`/api/v2\`, from ${V2_FIRST_RELEASE}. A running server lists the same table at \`GET /api/v2\`, which needs no credential. \`/api/v1\` is deprecated and ends on the date a deployment sets: [HTTP API v1](/legion/dispatch/reference/api-v1/) lists its routes and says how it ends.`,
    "",
    "## What every route keeps",
    "",
    "- A list answers `{items, total, limit, offset}`. A `limit` outside the route's bounds is refused `400 INVALID_LIMIT`, and an `offset` that is not a whole number of 0 or more `400 INVALID_OFFSET`. The global event log (`GET /api/v2/events`) and the caller's dismissed threads (`GET /api/v2/me/threads?state=dismissed`) answer `total: null`, since counting either costs more than its page; every other list answers an exact `total`.",
    "- A log reads `after` and `before` in its one position field. An `after` past the log's last position, and a value not written as that field is, are refused `400 POSITION_INVALID`, naming the parameter and the field.",
    "- A refusal is `{code, error}`. Each code is one of one list the server declares, and one condition takes one code on every route.",
    "- A query key the route does not declare is refused `400 QUERY_KEY_UNKNOWN`, whose body also names the `key` and every key the route `declared`. A body member the route does not declare is ignored.",
    "- Every `POST`, `PATCH`, `PUT` and `DELETE` takes an `Idempotency-Key` header. A repeat under the same key answers what the first request answered, marked `Idempotency-Replayed: ?1`, and runs nothing again; the mint of a personal agent token, whose answer holds the token, answers a repeat `409 KEY_COMMITTED` instead.",
    "- A `PUT` whose whole body equals the object's present state and fields, sent by whoever set them, answers `200` with the object and writes nothing.",
    "- A path that names an object Dispatch stores, which does not exist, answers `404` with that object's code, such as `ISSUE_NOT_FOUND`. A `{session_id}` and a repository's app installation keep the answer `/api/v1` gives: Dispatch cannot tell a session that never existed from one that has gone, and GitHub answers alike for a missing repository and one the App cannot see.",
    "- A path no route serves answers `404 NO_ROUTE`.",
    "",
    ...(unbound === ""
      ? []
      : [
          `The routes at ${unbound} keep the shape and the answers they have at the same path under \`/api/v1\`, so none of these rules binds them, and they name nothing of what they serve below.`,
          "",
        ]),
    ...callerSections(),
    "## What a route serves",
    "",
    "Each route below names the resource it serves and what kind of thing that is:",
    "",
    ...Object.values(KINDS).map(({ label, meaning }) => `- **${label}**: ${meaning}`),
    "",
    ...groupedRoutes(routes, routeLinesV2),
  ].join("\n");
}

function renderV1(routes: Route[]): string {
  return [
    `\`/api/v1\` is deprecated. [HTTP API](/legion/dispatch/reference/api/) lists the \`/api/v2\` routes that replace it, which Dispatch serves from ${V2_FIRST_RELEASE}. From that release on, each \`/api/v1\` answer carries a \`Deprecation\` header (RFC 9745) and a \`Link\` header with \`rel="successor-version"\` that names the \`/api/v2\` route answering for it.`,
    "",
    "A deployment ends `/api/v1` at an instant it sets: `DISPATCH_API_V1_END` on the server, which the `gcp:Dispatch` component's `apiV1End` input sets. While that instant is ahead, each `/api/v1` answer carries a `Sunset` header (RFC 8594) naming it. Once it has passed, every path under `/api/v1` answers `410 API_V1_ENDED` and points to `GET /api/v2`. With the setting unset, `/api/v1` has no end.",
    "",
    `Dispatch serves ${routes.length} routes under \`/api/v1\`. A running server lists the same table at \`GET /api/v1\`, which needs no credential.`,
    "",
    ...callerSections(),
    ...groupedRoutes(routes, () => []),
  ].join("\n");
}

writePage({
  path: "dispatch/reference/api.md",
  title: "HTTP API",
  description:
    "Every route Dispatch serves under /api/v2: method, path, who may call it, the OAuth scopes it needs, what it serves and how a client pages it, and what it does.",
  sources: ["packages/envoy/internal/dispatch/api/routes_table.go"],
  body: () => renderV2(readRoutesV2()),
});

writePage({
  path: "dispatch/reference/api-v1.md",
  title: "HTTP API v1 (deprecated)",
  description:
    "Every route Dispatch still serves under the deprecated /api/v1, until the end a deployment sets: method, path, who may call it, the OAuth scopes it needs, and what it does.",
  sources: ["packages/envoy/internal/dispatch/api/routes_table.go"],
  body: () => renderV1(readRoutesV1()),
});
