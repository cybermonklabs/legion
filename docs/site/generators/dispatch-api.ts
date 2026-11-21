#!/usr/bin/env bun
// Writes <content dir>/dispatch/reference/api.md from Dispatch's route table
// (packages/envoy/internal/dispatch/api/routes_table.go), read through `envoy-dispatch routes`,
// which prints the body GET /api/v1 serves without a database or a listener. Contract:
// scripts/generate.ts; lib/reference.ts runs the binary.
import { inline, writePage } from "./lib/markdown.ts";
import { readTable } from "./lib/reference.ts";

/** Who may call a route, by the table's auth value (`routeAuth` in the source). */
const CALLERS: Record<string, { label: string; meaning: string }> = {
  any: {
    label: "Human or agent",
    meaning: "a signed-in human (browser session) or an agent (bearer token).",
  },
  bearer: {
    label: "Agent only",
    meaning: "an agent's bearer token; a browser session is refused.",
  },
};

/** What each OAuth scope a route can name lets the credential holding it do (`routeScopes` in
 *  the source, `model.ScopePerson` and `model.ScopeApprove`). */
const SCOPES: Record<string, string> = {
  person:
    "this credential acts as its person. A browser sign-in holds it, so does a personal token unless its owner took it away, and so does a token the secrets broker issued that acts for its person.",
  approve:
    "beside `person`, this credential may do what settles work or controls a credential: a document review, keeping a decision, the answer of an ask of kind `approval`, a machine-login decision, a credential-grant, machine-login or sign-in revoke, minting, changing or revoking a personal token, and a change to a project's access list, visibility, owner or Slack channels or to a group an access list can name. A browser sign-in holds it, and so does a personal token its owner gave it; no token the secrets broker issued holds it, and no credential holds `approve` without `person`.",
};

interface Route {
  method: string;
  path: string;
  auth: string;
  scopes?: string[];
  description: string;
}

function readRoutes(): Route[] {
  return readTable("envoy-dispatch", ["routes"], "routes", [
    "method",
    "path",
    "auth",
    "description",
  ]).map((route) => {
    if (!Object.hasOwn(CALLERS, route.auth as string)) {
      throw new Error(`route ${route.method} ${route.path} has unknown auth ${route.auth}`);
    }
    // A route that needs no scope leaves the field out, so only a present one is read.
    if (route.scopes !== undefined) {
      if (!Array.isArray(route.scopes) || route.scopes.length === 0) {
        throw new Error(`route ${route.method} ${route.path} has unreadable scopes`);
      }
      for (const scope of route.scopes) {
        if (typeof scope !== "string" || !Object.hasOwn(SCOPES, scope)) {
          throw new Error(`route ${route.method} ${route.path} needs unknown scope ${scope}`);
        }
      }
    }
    return route as unknown as Route;
  });
}

/** The scopes of one route as its line names them: `` `person` `` or `` `person` and `approve` ``. */
function scopeNames(scopes: readonly string[]): string {
  const names = scopes.map((scope) => `\`${scope}\``);
  return names.length === 1 ? names[0] : `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`;
}

/** Whether a path is one of the routes under `/api/v1`, the index itself included. */
function underAPI(path: string): boolean {
  return path === "/api/v1" || path.startsWith("/api/v1/");
}

function render(routes: Route[]): string {
  // A route belongs to the resource its path names after /api/v1: /api/v1/issues/{key}/asks is
  // an issues route, and /api/v1 itself is the index (""). A route of the table outside /api/v1
  // belongs to its own first segment: GET /metrics is a metrics route.
  const groups = Map.groupBy(routes, (route) =>
    underAPI(route.path) ? (route.path.split("/")[3] ?? "") : (route.path.split("/")[1] ?? "")
  );
  const api = routes.filter((route) => underAPI(route.path));
  const beside = routes
    .filter((route) => !underAPI(route.path))
    .map((route) => `\`${route.method} ${route.path}\``);
  const besideSentence = beside.length === 0 ? "" : `, and ${beside.join(", ")} beside them`;
  const lines = [
    `Dispatch serves ${api.length} routes under \`/api/v1\`${besideSentence}. A running server lists the same table at \`GET /api/v1\` to any signed-in person or agent credential.`,
    "",
    "## Who may call a route",
    "",
    ...Object.values(CALLERS).map(({ label, meaning }) => `- **${label}**: ${meaning}`),
    "",
    "## The scopes a route needs",
    "",
    'A route can need OAuth scopes of the credential that calls it, and the credential must hold every one it names. A credential that holds none — the shared agent token a deployment configures, a Kubernetes service-account token, a broker-issued token scoped to a project — is refused `403 OAUTH_SCOPE_REQUIRED` naming the scope, under `WWW-Authenticate: Bearer error="insufficient_scope", scope="<scope>"` (RFC 6750 section 3.1).',
    "",
    ...Object.entries(SCOPES).map(([scope, meaning]) => `- \`${scope}\`: ${meaning}`),
    "",
    "A read-only token the secrets broker issued holds neither scope. Dispatch serves it only on the routes `readScopeRoutes` in the source lists, and refuses it `403 READ_ONLY_TOKEN` on every other route, with no challenge header, before it reads the route's scopes. The route index does not carry that list, so this page does not mark those routes.",
    "",
    "Path parameters are the `{name}` segments of a path.",
    "",
  ];
  for (const resource of [...groups.keys()].sort()) {
    const words = resource.replaceAll("-", " ");
    const title = resource === "" ? "Index" : words.charAt(0).toUpperCase() + words.slice(1);
    lines.push(`## ${title}`, "");
    for (const route of groups.get(resource) ?? []) {
      lines.push(`### ${route.method} ${route.path}`, "", inline(route.description), "");
      lines.push(`- **Who may call:** ${CALLERS[route.auth]?.label}`);
      if (route.scopes !== undefined) {
        lines.push(`- **Needs the scope:** ${scopeNames(route.scopes)}`);
      }
      const parameters = [...route.path.matchAll(/\{([^}]+)\}/g)].map((match) => `\`${match[1]}\``);
      if (parameters.length > 0) {
        lines.push(`- **Path parameters:** ${parameters.join(", ")}`);
      }
      lines.push("");
    }
  }
  return lines.join("\n");
}

writePage({
  path: "dispatch/reference/api.md",
  title: "HTTP API",
  description:
    "Every route of Dispatch's route table, under /api/v1 and beside it: method, path, who may call it, the OAuth scopes it needs, and what it does.",
  sources: ["packages/envoy/internal/dispatch/api/routes_table.go"],
  body: () => render(readRoutes()),
});
