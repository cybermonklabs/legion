import { expect, test } from "bun:test";
import { requestCredentials } from "./remote-text";

// The rows of Go's public_routes_test.go, the same requests read through requestCredentials: a
// value under a name the public-name table lists is public on a route its contract names, and a
// credential like any other on every other route.

/** One value a request carries under a listed name, sent with `method` to `target`, in its query,
 *  its form, its JSON body (a string, or a list of one string for `json-list`) or a header; `on`
 *  says whether the route is one the name's contract names. */
type RoutedValue = readonly [
  method: string,
  target: string,
  at: "query" | "form" | "json" | "json-list" | "header",
  name: string,
  value: string,
  on: boolean,
];

/** Requires each of `values` to be among its request's credentials off its route and not on it. */
function requireRoutes(values: readonly RoutedValue[]): void {
  for (const [method, target, at, name, value, on] of values) {
    let url = target;
    const headers: Record<string, string> = {};
    let body: unknown;
    if (at === "query") {
      url += `?${new URLSearchParams({ [name]: value })}`;
    } else if (at === "form") {
      headers["content-type"] = "application/x-www-form-urlencoded";
      body = new URLSearchParams({ [name]: value });
    } else if (at === "header") {
      headers[name] = value;
    } else {
      headers["content-type"] = "application/json";
      body = JSON.stringify({ [name]: at === "json-list" ? [value] : value });
    }
    const credentials = requestCredentials(url, { method, headers, body });
    expect(
      credentials.includes(value),
      `${method} ${target} ${name}: ${on ? "public on" : "a credential off"} its route`
    ).toBe(!on);
  }
}

const listener = "http://127.0.0.1:7777";
const broker = "https://broker.example";
const daemon = "http://127.0.0.1:7600";
const iam = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/";
const metadata = "http://169.254.169.254";
const dispatch = "https://dispatch.example";

// Go's TestAListenerMessagesNamesArePublicOnlyOnTheSendAndPublishRoutes.
test("a listener message's names are public only on the send and publish routes", () => {
  const send = `${listener}/v1/messages/send`;
  const publish = `${listener}/v1/messages/publish`;
  requireRoutes([
    ["POST", send, "json", "target_session", "session-sent-to", true],
    ["POST", send, "json", "message", "message-text-words", true],
    ["POST", send, "json", "payload", "payload-frame-words", true],
    ["POST", send, "json", "expects_reply", "required", true],
    ["POST", publish, "json", "topic", "topic-published-to", true],
    ["POST", publish, "json", "dedupe_key", "dedupe-key-words", true],
    ["POST", publish, "json", "message", "message-text-words", true],
    ["POST", publish, "json", "expects_reply", "optional", true],
    ["POST", publish, "json", "target_session", "session-sent-to", false],
    ["POST", send, "json", "topic", "topic-published-to", false],
    ["POST", send, "json", "dedupe_key", "dedupe-key-words", false],
    ["GET", send, "query", "message", "message-text-words", false],
    ["POST", `${send}/again`, "json", "message", "message-text-words", false],
    ["POST", `${listener}/api/v1/messages/send`, "json", "payload", "payload-frame-words", false],
    [
      "POST",
      `${daemon}/legion/v1/handoff/complete`,
      "json",
      "message",
      "message-text-words",
      false,
    ],
  ]);
});

// Go's TestAListenerReadsNamesArePublicOnlyOnItsRegistryReads.
test("a listener read's names are public only on its registry reads", () => {
  requireRoutes([
    ["GET", `${listener}/v1/sessions`, "query", "include", "registry-states", true],
    ["GET", `${listener}/v1/roles/worker-role`, "query", "include", "registry-states", true],
    ["GET", `${listener}/v1/registry/entry-name`, "query", "include", "registry-states", true],
    ["GET", `${listener}/v1/legion/agents`, "query", "issue", "issue-key-read", true],
    ["GET", `${listener}/v1/sessions/session-name`, "query", "include", "registry-states", false],
    ["GET", `${listener}/v1/roles/worker/role`, "query", "include", "registry-states", false],
    ["POST", `${listener}/v1/sessions`, "query", "include", "registry-states", false],
    ["GET", `${listener}/v1/legion/agents`, "query", "include", "registry-states", false],
    ["GET", `${listener}/v1/sessions`, "query", "issue", "issue-key-read", false],
    ["POST", `${daemon}/legion/v1/operator/claims`, "json", "issue", "issue-key-read", false],
  ]);
});

// Go's TestARoleIsPublicOnlyOnTheDaemonsOperatorClaimRoutes.
test("a role is public only on the daemon's operator claim routes", () => {
  const claims = `${daemon}/legion/v1/operator/claims`;
  requireRoutes([
    ["POST", claims, "json", "role", "worker-role-name", true],
    ["POST", `${claims}/claim-token/deliver`, "json", "role", "worker-role-name", true],
    ["GET", claims, "query", "role", "worker-role-name", false],
    ["POST", `${daemon}/legion/v1/claims/fresh-session`, "json", "role", "worker-role-name", false],
    ["POST", `${daemon}/legion/v1/operator/claimsets`, "json", "role", "worker-role-name", false],
    ["POST", `${listener}/v1/messages/send`, "json", "role", "worker-role-name", false],
  ]);
});

// Go's TestAnEnrollmentsNamesArePublicOnlyOnPostEnrollments.
test("an enrollment's names are public only on POST /v1/enrollments", () => {
  const enrollments = `${broker}/v1/enrollments`;
  requireRoutes([
    ["POST", enrollments, "json", "kind", "enrollment-kind", true],
    ["POST", enrollments, "json", "runtime_id", "runtime-named", true],
    ["POST", enrollments, "json", "session_id", "session-enrolled", true],
    ["POST", enrollments, "json", "operator", "operator-sign-in", true],
    ["POST", enrollments, "json", "thumbprint", "thumbprint-of-key", true],
    ["POST", enrollments, "json-list", "projects", "PROJECTKEY", true],
    ["POST", `${enrollments}/enrollment-id`, "json", "session_id", "session-enrolled", false],
    ["GET", enrollments, "query", "kind", "enrollment-kind", false],
    ["POST", `${broker}/v1/requests`, "json", "operator", "operator-sign-in", false],
    ["POST", `${broker}/v1/oauth/token`, "form", "thumbprint", "thumbprint-of-key", false],
    ["POST", `${enrollments}/carried`, "json-list", "projects", "PROJECTKEY", false],
    ["GET", enrollments, "query", "projects", "PROJECTKEY", false],
    [
      "POST",
      `${broker}/v1/oauth/exchange/pod-sessions`,
      "json",
      "thumbprint",
      "thumbprint-of-key",
      false,
    ],
  ]);
});

// Go's TestTheBrokersRequestAndReleaseNamesArePublicOnlyOnTheirRoutes.
test("the broker's request and release names are public only on their routes", () => {
  const requests = `${broker}/v1/requests`;
  const values = `${broker}/v1/grants/grant-id/values`;
  requireRoutes([
    ["GET", `${requests}/request-id`, "query", "wait", "30000000", true],
    ["POST", requests, "json", "owner", "owner-narrowed", true],
    ["POST", requests, "json-list", "repositories", "repository-narrowed", true],
    ["POST", values, "json", "owner", "owner-narrowed", true],
    ["POST", values, "json-list", "repositories", "repository-narrowed", true],
    ["POST", values, "json", "reason", "release-reason-words", true],
    ["POST", values, "json", "command", "release-command-words", true],
    ["GET", requests, "query", "wait", "30000000", false],
    ["POST", `${requests}/request-id`, "query", "wait", "30000000", false],
    ["POST", requests, "json", "reason", "release-reason-words", false],
    ["POST", requests, "json", "command", "release-command-words", false],
    ["POST", `${broker}/v1/grants/grant-id/revoke`, "json", "owner", "owner-narrowed", false],
    ["GET", values, "query", "repositories", "repository-narrowed", false],
    ["POST", "https://api.github.com/graphql", "json", "owner", "owner-narrowed", false],
  ]);
});

// Go's TestATokenExchangesNamesArePublicOnlyOnItsRoutes.
test("a token exchange's names are public only on its routes", () => {
  const tokenRoute = `${broker}/v1/oauth/token`;
  const sts = "https://sts.googleapis.com/v1/token";
  const identity = `${metadata}/computeMetadata/v1/instance/service-accounts/default/identity`;
  requireRoutes([
    ["POST", tokenRoute, "form", "session", "worker-session", true],
    ["POST", tokenRoute, "form", "projects", "project-keys", true],
    ["POST", tokenRoute, "form", "launcher_credential", "launcher-false", true],
    ["POST", tokenRoute, "form", "grant_type", "token-exchange-grant", true],
    ["POST", tokenRoute, "form", "subject_token_type", "subject-type-name", true],
    ["POST", tokenRoute, "form", "actor_token_type", "actor-type-name", true],
    ["POST", sts, "form", "requested_token_type", "requested-type-name", true],
    ["POST", sts, "form", "audience", "audience-of-token", true],
    ["GET", "https://idp.example/authorize", "query", "client_id", "client-identifier", true],
    ["GET", "https://idp.example/authorize", "query", "scope", "openid-profile", true],
    ["POST", `${broker}/token`, "form", "session", "worker-session", false],
    ["GET", tokenRoute, "query", "session", "worker-session", false],
    [
      "POST",
      `${broker}/v1/oauth/exchange/pod-sessions`,
      "json",
      "session",
      "worker-session",
      false,
    ],
    ["POST", `${broker}/v1/oauth/revoke`, "form", "launcher_credential", "launcher-false", false],
    ["GET", identity, "query", "audience", "audience-of-token", false],
    ["GET", tokenRoute, "query", "grant_type", "token-exchange-grant", false],
    ["PUT", sts, "form", "subject_token_type", "subject-type-name", false],
  ]);
});

// Go's TestAnImpersonationsNamesArePublicOnlyOnGenerateAccessToken.
test("an impersonation's names are public only on generateAccessToken", () => {
  const account = `${iam}writer@example-project.iam.gserviceaccount.com`;
  requireRoutes([
    ["POST", `${account}:generateAccessToken`, "json-list", "delegates", "chain-account-one", true],
    ["POST", `${account}:generateAccessToken`, "json", "lifetime", "3600.000s", true],
    ["POST", `${account}:generateIdToken`, "json-list", "delegates", "chain-account-one", false],
    ["GET", `${account}:generateAccessToken`, "query", "lifetime", "3600.000s", false],
    ["POST", account, "json", "lifetime", "3600.000s", false],
    ["POST", `${iam}:generateAccessToken`, "json", "lifetime", "3600.000s", false],
  ]);
});

// Go's TestAPageNumberIsPublicOnlyOnAGet.
test("a page number is public only on a GET", () => {
  requireRoutes([
    ["GET", "https://api.github.com/installation/repositories", "query", "page", "10000001", true],
    [
      "GET",
      "https://github.example/api/v3/repos/acme/widgets/pulls",
      "query",
      "per_page",
      "10000002",
      true,
    ],
    ["POST", "https://api.github.com/graphql", "json", "page", "10000001", false],
    [
      "POST",
      "https://api.github.com/repos/acme/widgets/issues",
      "form",
      "per_page",
      "10000002",
      false,
    ],
  ]);
});

// Go's TestAMetadataFlavorIsPublicOnlyOnAMetadataRead.
test("a Metadata-Flavor is public only on a metadata read", () => {
  const tokenRoute = `${metadata}/computeMetadata/v1/instance/service-accounts/default/token`;
  requireRoutes([
    ["GET", tokenRoute, "header", "Metadata-Flavor", "flavor-of-google", true],
    [
      "GET",
      `${metadata}/computeMetadata/v1`,
      "header",
      "Metadata-Flavor",
      "flavor-of-google",
      true,
    ],
    [
      "PUT",
      `${metadata}/computeMetadata/v1/instance/guest-attributes/key`,
      "header",
      "Metadata-Flavor",
      "flavor-of-google",
      false,
    ],
    ["GET", `${metadata}/`, "header", "Metadata-Flavor", "flavor-of-google", false],
    ["GET", `${broker}/v1/token`, "header", "Metadata-Flavor", "flavor-of-google", false],
  ]);
});

// Go's TestADaemonsDispatchWriteNamesArePublicOnlyOnItsWriteRoutes.
test("a daemon's Dispatch write names are public only on its write routes", () => {
  const issue = `${dispatch}/api/v1/issues/issue-key-path`;
  const project = `${dispatch}/api/v1/projects/project-key-path`;
  const step = `${issue}/steps/step-record-id`;
  requireRoutes([
    ["DELETE", `${issue}/claim`, "json", "kind", "writer-kind-word", true],
    ["POST", `${issue}/messages`, "json", "kind", "writer-kind-word", true],
    ["POST", `${issue}/artifacts`, "json", "kind", "writer-kind-word", true],
    ["PUT", `${project}/gated-steps`, "json", "kind", "writer-kind-word", true],
    ["PUT", `${issue}/design-gates`, "json", "kind", "writer-kind-word", true],
    ["PUT", `${issue}/role-holders`, "json", "kind", "writer-kind-word", true],
    ["PUT", step, "json", "kind", "writer-kind-word", true],
    ["PUT", `${step}/rating`, "json", "kind", "writer-kind-word", true],
    ["PUT", `${issue}/plans/plan-record-id`, "json", "kind", "writer-kind-word", true],
    ["PUT", `${issue}/role-holders`, "json", "role", "worker-role-name", true],
    ["PUT", step, "json", "role", "worker-role-name", true],
    ["PUT", `${step}/rating`, "json", "role", "worker-role-name", true],
    ["PATCH", issue, "json", "kind", "writer-kind-word", false],
    ["POST", `${issue}/claim`, "json", "kind", "writer-kind-word", false],
    ["PUT", `${issue}/role-holders/extra`, "json", "kind", "writer-kind-word", false],
    ["PUT", `${issue}/steps`, "json", "kind", "writer-kind-word", false],
    ["POST", `${issue}/comments`, "json", "kind", "writer-kind-word", false],
    ["DELETE", `${issue}/claim`, "json", "role", "worker-role-name", false],
    ["PUT", `${issue}/role-holders`, "json", "session", "session-held-name", false],
    ["PUT", step, "json", "session", "session-held-name", false],
  ]);
});
