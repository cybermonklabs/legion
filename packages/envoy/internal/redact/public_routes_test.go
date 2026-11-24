package redact_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
)

// routedValue is one value a request carries under a name the public-name table lists, the
// request sent with method to target and the value in its query, its form, its JSON body (a
// string, or a list of one string for "json-list") or a header (at). on says whether the route is
// one the name's contract names: there the answer's words print the value, and on any other route
// the value is a credential like any other. The TypeScript rows of public-request-routes.test.ts
// send the same requests.
type routedValue struct {
	method, target, at, name, value string
	on                              bool
}

// requireRoutes reads, for each of values, an answer to its request that quotes the value
// (ReadAnswer, then Words), and requires the words to hold the value on its route and to hold it
// redacted off it.
func requireRoutes(t *testing.T, values []routedValue) {
	t.Helper()
	for _, v := range values {
		route := "on"
		if !v.on {
			route = "off"
		}
		t.Run(route+" "+v.method+" "+v.target+" "+v.name, func(t *testing.T) {
			words := routedWords(t, v)
			if v.on && !strings.Contains(words, v.value) {
				t.Errorf("the words are %q; want %s's value %q as the answer wrote it, on a route its contract names", words, v.name, v.value)
			}
			if !v.on && (strings.Contains(words, v.value) || !strings.Contains(words, redact.Redacted)) {
				t.Errorf("the words are %q; want %s's value %q redacted, on a route its contract does not name", words, v.name, v.value)
			}
		})
	}
}

// routedWords is the words of a refusal of v's request that quotes v's value.
func routedWords(t *testing.T, v routedValue) string {
	t.Helper()
	target, body, headers := v.target, "", map[string]string{}
	switch v.at {
	case "query":
		target += "?" + url.Values{v.name: {v.value}}.Encode()
	case "form":
		body, headers["Content-Type"] = url.Values{v.name: {v.value}}.Encode(), "application/x-www-form-urlencoded"
	case "json", "json-list":
		var member any = v.value
		if v.at == "json-list" {
			member = []string{v.value}
		}
		encoded, err := json.Marshal(map[string]any{v.name: member})
		if err != nil {
			t.Fatal(err)
		}
		body, headers["Content-Type"] = string(encoded), "application/json"
	case "header":
		headers[v.name] = v.value
	default:
		t.Fatalf("no place %q", v.at)
	}
	said := "the request's " + v.name + " " + v.value + " was refused"
	answer, err := redact.ReadAnswer(&http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(said)), Request: newRequest(t, v.method, target, body, headers)}, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	return answer.Words().String()
}

const (
	listenerURL = "http://127.0.0.1:7777"
	brokerURL   = "https://broker.example"
	daemonURL   = "http://127.0.0.1:7600"
	iamURL      = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/"
	metadataURL = "http://169.254.169.254"
	dispatchURL = "https://dispatch.example"
)

// TestAListenerMessagesNamesArePublicOnlyOnTheSendAndPublishRoutes: a send's target_session, and
// a send's or a publish's message, payload and expects_reply, and a publish's topic and
// dedupe_key, print as the listener wrote them back on POST /v1/messages/send and POST
// /v1/messages/publish, as listenerwire declares them; the same names on the other route, on
// another method, below the route, under a base path or on another server's route are credentials.
func TestAListenerMessagesNamesArePublicOnlyOnTheSendAndPublishRoutes(t *testing.T) {
	send, publish := listenerURL+"/v1/messages/send", listenerURL+"/v1/messages/publish"
	requireRoutes(t, []routedValue{
		{http.MethodPost, send, "json", "target_session", "session-sent-to", true},
		{http.MethodPost, send, "json", "message", "message-text-words", true},
		{http.MethodPost, send, "json", "payload", "payload-frame-words", true},
		{http.MethodPost, send, "json", "expects_reply", "required", true},
		{http.MethodPost, publish, "json", "topic", "topic-published-to", true},
		{http.MethodPost, publish, "json", "dedupe_key", "dedupe-key-words", true},
		{http.MethodPost, publish, "json", "message", "message-text-words", true},
		{http.MethodPost, publish, "json", "expects_reply", "optional", true},
		{http.MethodPost, publish, "json", "target_session", "session-sent-to", false},
		{http.MethodPost, send, "json", "topic", "topic-published-to", false},
		{http.MethodPost, send, "json", "dedupe_key", "dedupe-key-words", false},
		{http.MethodGet, send, "query", "message", "message-text-words", false},
		{http.MethodPost, send + "/again", "json", "message", "message-text-words", false},
		{http.MethodPost, listenerURL + "/api/v1/messages/send", "json", "payload", "payload-frame-words", false},
		{http.MethodPost, daemonURL + "/legion/v1/handoff/complete", "json", "message", "message-text-words", false},
	})
}

// TestAListenerReadsNamesArePublicOnlyOnItsRegistryReads: include prints on GET /v1/sessions, GET
// /v1/roles/{role} and GET /v1/registry/{id}, and issue on GET /v1/legion/agents, the listener's
// reads that take them; below a route, with two segments for its one, on another method or on
// another of the reads, each is a credential, and so is the issue a person types into the Legion
// daemon's spawn (POST /legion/v1/operator/claims), which nothing checks before it is sent.
func TestAListenerReadsNamesArePublicOnlyOnItsRegistryReads(t *testing.T) {
	requireRoutes(t, []routedValue{
		{http.MethodGet, listenerURL + "/v1/sessions", "query", "include", "registry-states", true},
		{http.MethodGet, listenerURL + "/v1/roles/worker-role", "query", "include", "registry-states", true},
		{http.MethodGet, listenerURL + "/v1/registry/entry-name", "query", "include", "registry-states", true},
		{http.MethodGet, listenerURL + "/v1/legion/agents", "query", "issue", "issue-key-read", true},
		{http.MethodGet, listenerURL + "/v1/sessions/session-name", "query", "include", "registry-states", false},
		{http.MethodGet, listenerURL + "/v1/roles/worker/role", "query", "include", "registry-states", false},
		{http.MethodPost, listenerURL + "/v1/sessions", "query", "include", "registry-states", false},
		{http.MethodGet, listenerURL + "/v1/legion/agents", "query", "include", "registry-states", false},
		{http.MethodGet, listenerURL + "/v1/sessions", "query", "issue", "issue-key-read", false},
		{http.MethodPost, daemonURL + "/legion/v1/operator/claims", "json", "issue", "issue-key-read", false},
	})
}

// TestARoleIsPublicOnlyOnTheDaemonsOperatorClaimRoutes: role prints on POST
// /legion/v1/operator/claims, the spawn, and on every POST below it, the routes of one claim; a
// GET of the list, another of the daemon's routes, a segment that only begins as the route's and
// the listener's send, whose role mark is the role a message's writer typed, make it a credential.
func TestARoleIsPublicOnlyOnTheDaemonsOperatorClaimRoutes(t *testing.T) {
	claims := daemonURL + "/legion/v1/operator/claims"
	requireRoutes(t, []routedValue{
		{http.MethodPost, claims, "json", "role", "worker-role-name", true},
		{http.MethodPost, claims + "/claim-token/deliver", "json", "role", "worker-role-name", true},
		{http.MethodGet, claims, "query", "role", "worker-role-name", false},
		{http.MethodPost, daemonURL + "/legion/v1/claims/fresh-session", "json", "role", "worker-role-name", false},
		{http.MethodPost, daemonURL + "/legion/v1/operator/claimsets", "json", "role", "worker-role-name", false},
		{http.MethodPost, listenerURL + "/v1/messages/send", "json", "role", "worker-role-name", false},
	})
}

// TestAnEnrollmentsNamesArePublicOnlyOnPostEnrollments: the kind, runtime, session, operator,
// thumbprint and projects an enrollment names print on POST /v1/enrollments; below it, on a GET of
// it, on the broker's other routes, the pod-session registration among them, they are credentials.
func TestAnEnrollmentsNamesArePublicOnlyOnPostEnrollments(t *testing.T) {
	enrollments := brokerURL + "/v1/enrollments"
	requireRoutes(t, []routedValue{
		{http.MethodPost, enrollments, "json", "kind", "enrollment-kind", true},
		{http.MethodPost, enrollments, "json", "runtime_id", "runtime-named", true},
		{http.MethodPost, enrollments, "json", "session_id", "session-enrolled", true},
		{http.MethodPost, enrollments, "json", "operator", "operator-sign-in", true},
		{http.MethodPost, enrollments, "json", "thumbprint", "thumbprint-of-key", true},
		{http.MethodPost, enrollments, "json-list", "projects", "PROJECTKEY", true},
		{http.MethodPost, enrollments + "/enrollment-id", "json", "session_id", "session-enrolled", false},
		{http.MethodGet, enrollments, "query", "kind", "enrollment-kind", false},
		{http.MethodPost, brokerURL + "/v1/requests", "json", "operator", "operator-sign-in", false},
		{http.MethodPost, brokerURL + "/v1/oauth/token", "form", "thumbprint", "thumbprint-of-key", false},
		{http.MethodPost, enrollments + "/carried", "json-list", "projects", "PROJECTKEY", false},
		{http.MethodGet, enrollments, "query", "projects", "PROJECTKEY", false},
		{http.MethodPost, brokerURL + "/v1/oauth/exchange/pod-sessions", "json", "thumbprint", "thumbprint-of-key", false},
	})
}

// TestTheBrokersRequestAndReleaseNamesArePublicOnlyOnTheirRoutes: wait prints on GET
// /v1/requests/{id}, owner and repositories on POST /v1/requests and POST /v1/grants/{id}/values,
// and reason and command on the release alone; on the list path, on another method, on a grant's
// other route and on the request each of the release's names is a credential, and so is the owner
// of a GitHub GraphQL query, which is GitHub's own spelling of a repository's owner.
func TestTheBrokersRequestAndReleaseNamesArePublicOnlyOnTheirRoutes(t *testing.T) {
	requests, values := brokerURL+"/v1/requests", brokerURL+"/v1/grants/grant-id/values"
	requireRoutes(t, []routedValue{
		{http.MethodGet, requests + "/request-id", "query", "wait", "30000000", true},
		{http.MethodPost, requests, "json", "owner", "owner-narrowed", true},
		{http.MethodPost, requests, "json-list", "repositories", "repository-narrowed", true},
		{http.MethodPost, values, "json", "owner", "owner-narrowed", true},
		{http.MethodPost, values, "json-list", "repositories", "repository-narrowed", true},
		{http.MethodPost, values, "json", "reason", "release-reason-words", true},
		{http.MethodPost, values, "json", "command", "release-command-words", true},
		{http.MethodGet, requests, "query", "wait", "30000000", false},
		{http.MethodPost, requests + "/request-id", "query", "wait", "30000000", false},
		{http.MethodPost, requests, "json", "reason", "release-reason-words", false},
		{http.MethodPost, requests, "json", "command", "release-command-words", false},
		{http.MethodPost, brokerURL + "/v1/grants/grant-id/revoke", "json", "owner", "owner-narrowed", false},
		{http.MethodGet, values, "query", "repositories", "repository-narrowed", false},
		{http.MethodPost, "https://api.github.com/graphql", "json", "owner", "owner-narrowed", false},
	})
}

// TestATokenExchangesNamesArePublicOnlyOnItsRoutes: the broker's session, projects and
// launcher_credential print on POST /v1/oauth/token, the broker's token endpoint, and the token
// request's own names (grant_type, the token types, audience) on a POST to any path, since each
// issuer sets its token endpoint's; client_id and scope, whose contract names no route, print on
// any. The broker's names on another path or on a GET, the pod-session registration's session (a
// name the pod's agent chose), a token request's names on a GET (a GET of the metadata server's
// identity token names its audience) and on a PUT, are credentials.
func TestATokenExchangesNamesArePublicOnlyOnItsRoutes(t *testing.T) {
	token, sts := brokerURL+"/v1/oauth/token", "https://sts.googleapis.com/v1/token"
	requireRoutes(t, []routedValue{
		{http.MethodPost, token, "form", "session", "worker-session", true},
		{http.MethodPost, token, "form", "projects", "project-keys", true},
		{http.MethodPost, token, "form", "launcher_credential", "launcher-false", true},
		{http.MethodPost, token, "form", "grant_type", "token-exchange-grant", true},
		{http.MethodPost, token, "form", "subject_token_type", "subject-type-name", true},
		{http.MethodPost, token, "form", "actor_token_type", "actor-type-name", true},
		{http.MethodPost, sts, "form", "requested_token_type", "requested-type-name", true},
		{http.MethodPost, sts, "form", "audience", "audience-of-token", true},
		{http.MethodGet, "https://idp.example/authorize", "query", "client_id", "client-identifier", true},
		{http.MethodGet, "https://idp.example/authorize", "query", "scope", "openid-profile", true},
		{http.MethodPost, brokerURL + "/token", "form", "session", "worker-session", false},
		{http.MethodGet, token, "query", "session", "worker-session", false},
		{http.MethodPost, brokerURL + "/v1/oauth/exchange/pod-sessions", "json", "session", "worker-session", false},
		{http.MethodPost, brokerURL + "/v1/oauth/revoke", "form", "launcher_credential", "launcher-false", false},
		{http.MethodGet, metadataURL + "/computeMetadata/v1/instance/service-accounts/default/identity", "query", "audience", "audience-of-token", false},
		{http.MethodGet, token, "query", "grant_type", "token-exchange-grant", false},
		{http.MethodPut, sts, "form", "subject_token_type", "subject-type-name", false},
	})
}

// TestAnImpersonationsNamesArePublicOnlyOnGenerateAccessToken: delegates and lifetime print on
// POST /v1/projects/{project}/serviceAccounts/{account}:generateAccessToken, the IAM Service Account
// Credentials method that defines them; on another of its methods, on a GET, on the account's
// own path and on a segment with no account before the method, they are credentials.
func TestAnImpersonationsNamesArePublicOnlyOnGenerateAccessToken(t *testing.T) {
	account := iamURL + "writer@example-project.iam.gserviceaccount.com"
	requireRoutes(t, []routedValue{
		{http.MethodPost, account + ":generateAccessToken", "json-list", "delegates", "chain-account-one", true},
		{http.MethodPost, account + ":generateAccessToken", "json", "lifetime", "3600.000s", true},
		{http.MethodPost, account + ":generateIdToken", "json-list", "delegates", "chain-account-one", false},
		{http.MethodGet, account + ":generateAccessToken", "query", "lifetime", "3600.000s", false},
		{http.MethodPost, account, "json", "lifetime", "3600.000s", false},
		{http.MethodPost, iamURL + ":generateAccessToken", "json", "lifetime", "3600.000s", false},
	})
}

// TestAPageNumberIsPublicOnlyOnAGet: page and per_page print on a GET of any path, a list read of
// GitHub's on whatever base path its deployment serves; in a POST's body they are credentials.
func TestAPageNumberIsPublicOnlyOnAGet(t *testing.T) {
	requireRoutes(t, []routedValue{
		{http.MethodGet, "https://api.github.com/installation/repositories", "query", "page", "10000001", true},
		{http.MethodGet, "https://github.example/api/v3/repos/acme/widgets/pulls", "query", "per_page", "10000002", true},
		{http.MethodPost, "https://api.github.com/graphql", "json", "page", "10000001", false},
		{http.MethodPost, "https://api.github.com/repos/acme/widgets/issues", "form", "per_page", "10000002", false},
	})
}

// TestAMetadataFlavorIsPublicOnlyOnAMetadataRead: Metadata-Flavor prints on a GET of
// /computeMetadata/v1 or any path below it, the metadata server's reads; on another method, on the
// server's root and on another server's path it is a credential.
func TestAMetadataFlavorIsPublicOnlyOnAMetadataRead(t *testing.T) {
	requireRoutes(t, []routedValue{
		{http.MethodGet, metadataURL + "/computeMetadata/v1/instance/service-accounts/default/token", "header", "Metadata-Flavor", "flavor-of-google", true},
		{http.MethodGet, metadataURL + "/computeMetadata/v1", "header", "Metadata-Flavor", "flavor-of-google", true},
		{http.MethodPut, metadataURL + "/computeMetadata/v1/instance/guest-attributes/key", "header", "Metadata-Flavor", "flavor-of-google", false},
		{http.MethodGet, metadataURL + "/", "header", "Metadata-Flavor", "flavor-of-google", false},
		{http.MethodGet, brokerURL + "/v1/token", "header", "Metadata-Flavor", "flavor-of-google", false},
	})
}

// TestADaemonsDispatchWriteNamesArePublicOnlyOnItsWriteRoutes: kind prints on the Dispatch writes
// whose every kind is the writer's own (the claim's release, a message, a document upload, the
// gated-steps, design-gates, role-holder, step, rating and plan writes: the actor's constant
// session, a step record's step, retro or merge), and role on the role-holder, step and rating
// writes, whose roles are the Legion daemon's role table's. kind on the issue's PATCH, whose
// external links' kinds are sent back as Dispatch answered them, on the claim route's POST, below a
// route, on a step list with no record id and on a comment, role on a write that names none, and
// the session a role holder or a step names, which the claim's agent chose, are credentials.
func TestADaemonsDispatchWriteNamesArePublicOnlyOnItsWriteRoutes(t *testing.T) {
	issue, project := dispatchURL+"/api/v1/issues/issue-key-path", dispatchURL+"/api/v1/projects/project-key-path"
	step := issue + "/steps/step-record-id"
	requireRoutes(t, []routedValue{
		{http.MethodDelete, issue + "/claim", "json", "kind", "writer-kind-word", true},
		{http.MethodPost, issue + "/messages", "json", "kind", "writer-kind-word", true},
		{http.MethodPost, issue + "/artifacts", "json", "kind", "writer-kind-word", true},
		{http.MethodPut, project + "/gated-steps", "json", "kind", "writer-kind-word", true},
		{http.MethodPut, issue + "/design-gates", "json", "kind", "writer-kind-word", true},
		{http.MethodPut, issue + "/role-holders", "json", "kind", "writer-kind-word", true},
		{http.MethodPut, step, "json", "kind", "writer-kind-word", true},
		{http.MethodPut, step + "/rating", "json", "kind", "writer-kind-word", true},
		{http.MethodPut, issue + "/plans/plan-record-id", "json", "kind", "writer-kind-word", true},
		{http.MethodPut, issue + "/role-holders", "json", "role", "worker-role-name", true},
		{http.MethodPut, step, "json", "role", "worker-role-name", true},
		{http.MethodPut, step + "/rating", "json", "role", "worker-role-name", true},
		{http.MethodPatch, issue, "json", "kind", "writer-kind-word", false},
		{http.MethodPost, issue + "/claim", "json", "kind", "writer-kind-word", false},
		{http.MethodPut, issue + "/role-holders/extra", "json", "kind", "writer-kind-word", false},
		{http.MethodPut, issue + "/steps", "json", "kind", "writer-kind-word", false},
		{http.MethodPost, issue + "/comments", "json", "kind", "writer-kind-word", false},
		{http.MethodDelete, issue + "/claim", "json", "role", "worker-role-name", false},
		{http.MethodPut, issue + "/role-holders", "json", "session", "session-held-name", false},
		{http.MethodPut, step, "json", "session", "session-held-name", false},
	})
}
