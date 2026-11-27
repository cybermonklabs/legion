#!/usr/bin/env bun
// Writes <content dir>/dispatch/reference/api.md from Dispatch's route declarations, one file a
// route in packages/envoy/internal/dispatch/api (route_<path>+<method>.go), read through
// `envoy-dispatch routes`, which prints the body GET /api/v1 serves without a database or a
// listener. Contract: scripts/generate.ts; lib/reference.ts runs the binary.
import type { DispatchScope } from "../../../packages/contracts/src/dispatch-api-core.ts";
import { inline, writePage } from "./lib/markdown.ts";
import { readTable } from "./lib/reference.ts";

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

/** What each OAuth scope a Dispatch credential can hold lets it do (each route's `OAuthScopes` in
 *  the source, `model.ScopePerson`, `model.ScopeApprove` and `model.ScopeP0`). `satisfies` ties the entries to
 *  `DISPATCH_SCOPES`: a scope added there with no entry here fails the typecheck. */
const SCOPES = {
  person:
    "this credential acts as its person. A browser sign-in holds it, so does a personal token unless its owner took it away, and so does a token the secrets broker issued that acts for its person.",
  approve:
    "beside `person`, this credential may do what settles work or controls a credential: a document review, keeping a decision, the answer of an ask of kind `approval`, a machine-login decision, a credential-grant, machine-login or sign-in revoke, and minting, changing or revoking a personal token. A browser sign-in holds it, and so does a personal token its owner gave it; of the tokens the secrets broker issues, only the approve-only token holds it, which no machine holds and which calls only the approve list's routes; and no credential holds `approve` without `person`.",
  p0: "opens no route. A priority of 0 on `POST /api/v1/issues` and `PATCH /api/v1/issues/{key}` needs it when the write makes the issue's own priority 0. Keeping P0, lowering it, clearing it and priorities 1 to 3 need no scope. A browser sign-in holds `p0`. A personal token holds it only where its maker gave it, and may hold it alone: no mint gives it by default. No broker token, shared token or service-account token holds `p0`.",
} satisfies Record<DispatchScope, string>;

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

function render(routes: Route[]): string {
  // A route belongs to the resource its path names after /api/v1: /api/v1/issues/{key}/asks is
  // an issues route, and /api/v1 itself is the index ("").
  const groups = Map.groupBy(routes, (route) => route.path.split("/")[3] ?? "");
  const lines = [
    `Dispatch serves ${routes.length} routes under \`/api/v1\`. A running server lists the same table at \`GET /api/v1\`, which needs no credential.`,
    "",
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
    "A read-only token is any token that holds `read`: one the secrets broker minted from a read entry or narrowed because its trade asked for `read`, or a personal token minted with `read`. It holds `person` at most, never `approve` or `p0`. Dispatch serves it only on the GET routes it classes as reads (each route's `Read` in the source), and refuses it `403 READ_ONLY_TOKEN` on every other route, with no challenge header, before it reads the route's scopes. It refuses a read-only token that carries a cut `403 AS_OF_TOKEN_NOT_SERVED` on every read but `GET /api/v1/whoami`. The route index does not carry the read class, so this page does not mark those routes.",
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
    "Every route Dispatch serves under /api/v1: method, path, who may call it, the OAuth scopes it needs, and what it does.",
  // The route files, one a route: the directory, since no one file holds the table any more.
  sources: ["packages/envoy/internal/dispatch/api"],
  body: () => render(readRoutes()),
});
