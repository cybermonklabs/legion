// The names of a request's values that a wire contract declares public, and the routes it
// declares them public on: every other value a request carries, under any name or under a listed
// name on a route its contract does not name, is a credential the words of its refusal must not
// print (`requestCredentials` here, `requestTokens` in Go). A name is here because the contract
// that defines it says its value is public, never because a test needs it, and each entry names
// that contract and, where the contract names them, the routes in its source's words. A name
// nobody listed counts as a credential, so a new parameter is redacted until a contract declares
// it public. Go reads this list from packages/envoy/internal/redact/public_names_generated.go,
// which `bun run gen:go` writes from it and `bun run check:go` holds to it;
// `public-request-names.test.ts` and Go's public_names_test.go fail on an entry with no source, on
// a route its source does not name, or on a name a credential travels under.

/** Where a request carries the value: a header, or a field of its query, form or JSON body. */
export type PublicNamePlace = "header" | "field";

/**
 * One name whose value is public, where it is carried, the contract that says so, and the routes
 * it says so on, each `"<METHOD> <PATH>"` ({@link routeMatches}): the value is public on a request
 * one of them matches and on no other. With no routes, as for a name the contract declares public
 * in every request that carries it, the value is public on every request.
 */
export type PublicRequestName = {
  readonly place: PublicNamePlace;
  readonly name: string;
  readonly source: string;
  readonly routes?: readonly string[];
};

/** RFC 6749 §3.2 and RFC 8693 §2.1: a token request is a POST, to a path each issuer sets. */
const TOKEN_REQUEST = ["POST /*"];

/** The broker's token endpoint, packages/envoy/internal/cipherclient/testdata/wire.json's. */
const BROKER_TOKEN = ["POST /v1/oauth/token"];

/** The broker's enrollment route. */
const ENROLLMENT = ["POST /v1/enrollments"];

/** The Dispatch writes whose every `kind` is the writer's own: the actor's, `session`, and a step
 *  record's, one of `step`, `retro` and `merge`. Not `PATCH /api/v1/issues/{key}`, whose
 *  `external_links[].kind` the dispatch command sends back as Dispatch answered it
 *  (packages/envoy-client/src/dispatch-commands/issues.ts:318,326). */
const DISPATCH_OWN_KIND = [
  "DELETE /api/v1/issues/{key}/claim",
  "POST /api/v1/issues/{key}/messages",
  "POST /api/v1/issues/{key}/artifacts",
  "PUT /api/v1/projects/{key}/gated-steps",
  "PUT /api/v1/issues/{key}/design-gates",
  "PUT /api/v1/issues/{key}/role-holders",
  "PUT /api/v1/issues/{key}/steps/{id}",
  "PUT /api/v1/issues/{key}/steps/{id}/rating",
  "PUT /api/v1/issues/{key}/plans/{id}",
];

/** The Dispatch writes that name a Legion role: an issue's role holders and a step record. */
const DISPATCH_ROLE = [
  "PUT /api/v1/issues/{key}/role-holders",
  "PUT /api/v1/issues/{key}/steps/{id}",
  "PUT /api/v1/issues/{key}/steps/{id}/rating",
];

/** The routes of the broker that narrow a GitHub App token: a request, and a grant's release. */
const NARROWING = ["POST /v1/requests", "POST /v1/grants/{id}/values"];

/** A grant's release, which records its reason and command. */
const RELEASE = ["POST /v1/grants/{id}/values"];

/** The Envoy listener's message routes. */
const SEND = ["POST /v1/messages/send"];
const PUBLISH = ["POST /v1/messages/publish"];

/** The IAM Service Account Credentials API's generateAccessToken. */
const GENERATE_ACCESS_TOKEN = [
  "POST /v1/projects/{project}/serviceAccounts/{account}:generateAccessToken",
];

/** GitHub's list requests: a GET, on a base path each deployment and test sets. */
const LIST_REQUEST = ["GET /*"];

export const PUBLIC_REQUEST_NAMES: readonly PublicRequestName[] = [
  { place: "header", name: "Accept", source: "RFC 9110 §12.5.1" },
  { place: "header", name: "Content-Length", source: "RFC 9110 §8.6" },
  { place: "header", name: "Content-Type", source: "RFC 9110 §8.3" },
  { place: "header", name: "User-Agent", source: "RFC 9110 §10.1.5" },
  {
    place: "header",
    name: "Metadata-Flavor",
    source:
      "GET /computeMetadata/v1/* (the Google Compute Engine metadata server): the header every request must carry, value Google",
    routes: ["GET /computeMetadata/v1/*"],
  },
  {
    place: "field",
    name: "grant_type",
    source:
      "POST /* (the token request, a POST by RFC 6749 §3.2 to the path its issuer sets): RFC 6749 §4.1.3, §6; RFC 8693 §2.1",
    routes: TOKEN_REQUEST,
  },
  {
    place: "field",
    name: "client_id",
    source: "RFC 6749 §2.2: a client identifier is not a secret",
  },
  { place: "field", name: "scope", source: "RFC 6749 §3.3" },
  {
    place: "field",
    name: "subject_token_type",
    source:
      "POST /* (the token exchange request, a POST by RFC 8693 §2.1 to the path its issuer sets): RFC 8693 §2.1",
    routes: TOKEN_REQUEST,
  },
  {
    place: "field",
    name: "actor_token_type",
    source:
      "POST /* (the token exchange request, a POST by RFC 8693 §2.1 to the path its issuer sets): RFC 8693 §2.1",
    routes: TOKEN_REQUEST,
  },
  {
    place: "field",
    name: "requested_token_type",
    source:
      "POST /* (the token exchange request, a POST by RFC 8693 §2.1 to the path its issuer sets): RFC 8693 §2.1",
    routes: TOKEN_REQUEST,
  },
  {
    place: "field",
    name: "audience",
    source:
      "POST /* (the token exchange request, a POST by RFC 8693 §2.1 to the path its issuer sets): RFC 8693 §2.1",
    routes: TOKEN_REQUEST,
  },
  { place: "field", name: "resource", source: "RFC 8693 §2.1; RFC 8707 §2" },
  {
    place: "field",
    name: "session",
    source:
      "POST /v1/oauth/token (the broker's token exchange, cipherclient/testdata/wire.json endpoint): form.session",
    routes: BROKER_TOKEN,
  },
  {
    place: "field",
    name: "projects",
    source:
      "POST /v1/oauth/token (the broker's token exchange, cipherclient/testdata/wire.json endpoint): form.projects; POST /v1/enrollments (the broker contract's enrollment projects): project keys the sender checked against the project-key rule before it sent them (packages/envoy/cmd/cipher/pod.go:141, the keys packages/envoy/cmd/cipher/register.go:216-220 parsed; packages/envoy/internal/cipher/helper/broker.go:1594, the keys packages/envoy/internal/cipher/helper/server.go:625-633 held)",
    routes: [...BROKER_TOKEN, ...ENROLLMENT],
  },
  {
    place: "field",
    name: "launcher_credential",
    source:
      "POST /v1/oauth/token (the broker's token exchange, cipherclient/testdata/wire.json endpoint): form.launcher_credential",
    routes: BROKER_TOKEN,
  },
  {
    place: "field",
    name: "kind",
    source:
      "POST /v1/enrollments: the broker contract's enrollment kind; DELETE /api/v1/issues/{key}/claim, POST /api/v1/issues/{key}/messages, POST /api/v1/issues/{key}/artifacts, PUT /api/v1/projects/{key}/gated-steps, PUT /api/v1/issues/{key}/design-gates, PUT /api/v1/issues/{key}/role-holders, PUT /api/v1/issues/{key}/steps/{id}, PUT /api/v1/issues/{key}/steps/{id}/rating and PUT /api/v1/issues/{key}/plans/{id} (Dispatch's issue writes): the writer's actor.kind, the constant session (the Legion daemon's packages/daemon/internal/dispatch/client.go:761, sent at :156, :284, :322, :170, :196, :214, :228, :243 and :261; the dispatch command's packages/envoy-client/src/dispatch-commands/command-context.ts:17), and a step record's kind, step, retro or merge (packages/daemon/internal/workflow/steps.go:413, sent at client.go:228 and :243)",
    routes: [...ENROLLMENT, ...DISPATCH_OWN_KIND],
  },
  {
    place: "field",
    name: "runtime_id",
    source: "POST /v1/enrollments: the broker contract's runtime the enrollment names",
    routes: ENROLLMENT,
  },
  {
    place: "field",
    name: "session_id",
    source: "POST /v1/enrollments: the broker contract's session the enrollment names",
    routes: ENROLLMENT,
  },
  {
    place: "field",
    name: "operator",
    source: "POST /v1/enrollments: the broker contract's operator, the person's sign-in",
    routes: ENROLLMENT,
  },
  {
    place: "field",
    name: "thumbprint",
    source: "POST /v1/enrollments: the RFC 7638 thumbprint of the session's public key",
    routes: ENROLLMENT,
  },
  {
    place: "field",
    name: "wait",
    source:
      "GET /v1/requests/{id}?wait=: the broker contract's hold on a pending request's read, in seconds",
    routes: ["GET /v1/requests/{id}"],
  },
  {
    place: "field",
    name: "owner",
    source:
      "POST /v1/requests and POST /v1/grants/{id}/values: the broker contract's GitHub App narrowing, the owner a token is narrowed to",
    routes: NARROWING,
  },
  {
    place: "field",
    name: "repositories",
    source:
      "POST /v1/requests and POST /v1/grants/{id}/values: the broker contract's GitHub App narrowing, the repositories a token is narrowed to",
    routes: NARROWING,
  },
  {
    place: "field",
    name: "reason",
    source:
      "POST /v1/grants/{id}/values: the broker contract's release reason, which the broker records with the release",
    routes: RELEASE,
  },
  {
    place: "field",
    name: "command",
    source:
      "POST /v1/grants/{id}/values: the broker contract's release command, which the broker records with the release",
    routes: RELEASE,
  },
  {
    place: "field",
    name: "include",
    source:
      "GET /v1/sessions, GET /v1/roles/{role} and GET /v1/registry/{id} (the Envoy listener, ?include=): the registry states a read asks for",
    routes: ["GET /v1/sessions", "GET /v1/roles/{role}", "GET /v1/registry/{id}"],
  },
  {
    place: "field",
    name: "issue",
    source:
      "GET /v1/legion/agents?issue= (the Envoy listener): the Dispatch issue key the Legion agent registry is read for",
    routes: ["GET /v1/legion/agents"],
  },
  {
    place: "field",
    name: "topic",
    source:
      "POST /v1/messages/publish (the Envoy listener, listenerwire.PublishRequest): the topic a message is published to",
    routes: PUBLISH,
  },
  {
    place: "field",
    name: "dedupe_key",
    source:
      "POST /v1/messages/publish (the Envoy listener, listenerwire.MessageBody): the idempotency key the listener's stream recognises a repeat of the message under",
    routes: PUBLISH,
  },
  {
    place: "field",
    name: "message",
    source:
      "POST /v1/messages/send and POST /v1/messages/publish (the Envoy listener, listenerwire.MessageBody): the message text the listener delivers to its recipient",
    routes: [...SEND, ...PUBLISH],
  },
  {
    place: "field",
    name: "payload",
    source:
      "POST /v1/messages/send and POST /v1/messages/publish (the Envoy listener, listenerwire.MessageBody): the event frame, a string, the listener delivers beside the message; it carries the message's text again",
    routes: [...SEND, ...PUBLISH],
  },
  {
    place: "field",
    name: "target_session",
    source:
      'POST /v1/messages/send (the Envoy listener, listenerwire.SendRequest): the session id the message is sent to, which the listener names in its answer ("no live session <id>")',
    routes: SEND,
  },
  {
    place: "field",
    name: "expects_reply",
    source:
      'POST /v1/messages/send and POST /v1/messages/publish (the Envoy listener, listenerwire.MessageBody): what the message wants back, false, "optional" or "required"',
    routes: [...SEND, ...PUBLISH],
  },
  {
    place: "field",
    name: "role",
    source:
      "POST /legion/v1/operator/claims/* (the Legion daemon's operator claim routes): the role a claim is spawned or read for; PUT /api/v1/issues/{key}/role-holders, PUT /api/v1/issues/{key}/steps/{id} and PUT /api/v1/issues/{key}/steps/{id}/rating (Dispatch's role-holder and step writes): a role of the Legion daemon's own role table, a claim's role (packages/daemon/internal/daemon/outbox.go:1508, sent at packages/daemon/internal/dispatch/client.go:214) or a step's (packages/daemon/internal/workflow/steps.go:48,50,54, sent at client.go:228 and :243)",
    routes: ["POST /legion/v1/operator/claims/*", ...DISPATCH_ROLE],
  },
  {
    place: "field",
    name: "delegates",
    source:
      "POST /v1/projects/{project}/serviceAccounts/{account}:generateAccessToken (IAM Service Account Credentials API, generateAccessToken): the service accounts of the chain",
    routes: GENERATE_ACCESS_TOKEN,
  },
  {
    place: "field",
    name: "lifetime",
    source:
      "POST /v1/projects/{project}/serviceAccounts/{account}:generateAccessToken (IAM Service Account Credentials API, generateAccessToken): the token's lifetime",
    routes: GENERATE_ACCESS_TOKEN,
  },
  {
    place: "field",
    name: "page",
    source:
      "GET /* (GitHub REST API, Using pagination in the REST API (docs.github.com/en/rest/using-the-rest-api/using-pagination-in-the-rest-api): a list request's query, a GET on the base path its deployment sets): the number of the page a list request reads",
    routes: LIST_REQUEST,
  },
  {
    place: "field",
    name: "per_page",
    source:
      "GET /* (GitHub REST API, Using pagination in the REST API (docs.github.com/en/rest/using-the-rest-api/using-pagination-in-the-rest-api): a list request's query, a GET on the base path its deployment sets): the number of results a page holds",
    routes: LIST_REQUEST,
  },
];

/** The route of the request a value is carried by, which an entry's routes are matched against:
 *  its method in upper case and its URL's path, or `undefined` for a URL that does not parse,
 *  which is on no route. */
export type RequestRoute = { readonly method: string; readonly path: string } | undefined;

/** The {@link RequestRoute} of a request to `url` sent with `method`, GET where it names none. */
export function requestRoute(url: string, method: string | undefined): RequestRoute {
  if (!URL.canParse(url)) return undefined;
  return { method: (method ?? "GET").toUpperCase(), path: new URL(url).pathname };
}

/** Whether the value under `name`, carried at `place` by a request on `route`, is public by its
 *  contract: a header's name compared without case, as HTTP compares it, a field's as written, on
 *  every route where the entry names none and on one of its routes where it does. */
export function isPublicRequestName(
  place: PublicNamePlace,
  name: string,
  route: RequestRoute
): boolean {
  return PUBLIC_REQUEST_NAMES.some(
    (entry) =>
      entry.place === place &&
      (place === "header"
        ? entry.name.toLowerCase() === name.toLowerCase()
        : entry.name === name) &&
      (entry.routes === undefined ||
        entry.routes.length === 0 ||
        (route !== undefined &&
          entry.routes.some((each) => routeMatches(each, route.method, route.path))))
  );
}

/** Whether `route`, `"<METHOD> <PATH>"`, matches a request's method, in upper case, and its whole
 *  path, compared segment by segment: a `{name}` in a segment matches one or more characters other
 *  than `/`, a last segment `*` matches the path above it and every path below it (`/*` every
 *  path), and every other character is literal. Go's `routeMatches`. */
export function routeMatches(route: string, method: string, path: string): boolean {
  const space = route.indexOf(" ");
  if (space < 0 || route.slice(0, space) !== method) return false;
  const want = route.slice(space + 1).split("/");
  const got = path.split("/");
  const below = want.at(-1) === "*";
  if (below) want.pop();
  if (below ? got.length < want.length : got.length !== want.length) return false;
  return want.every((segment, at) => segmentMatches(segment, got[at] as string));
}

/** Whether a path's `segment` matches a route's, `pattern`: each `{name}` of it matches one or more
 *  characters of `segment`, and every other character itself. Go's `segmentMatches`. */
function segmentMatches(pattern: string, segment: string): boolean {
  const open = pattern.indexOf("{");
  const end = open < 0 ? -1 : pattern.indexOf("}", open);
  if (end < 0) return pattern === segment;
  if (!segment.startsWith(pattern.slice(0, open))) return false;
  const rest = pattern.slice(end + 1);
  const tail = segment.slice(open);
  for (let taken = 1; taken <= tail.length; taken += 1) {
    if (segmentMatches(rest, tail.slice(taken))) return true;
  }
  return false;
}
