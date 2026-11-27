package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// mergeBaseSentences are every sentence of the two existing rows' descriptions at this slice's
// base (main as #4199's head 1cd4c83c88 holds it; GET /api/v1/whoami's as one entry, its whole
// description), each complete, in order: joined with a space they are that description byte for
// byte. c-blob-unchanged
// (dispatch://INFRA-3862/artifact/verification-md#b-c-blob-unchanged) allows a head to add
// sentences to an existing row's description that document a field or a refusal it adds, and
// fails on any other difference, "a changed or removed sentence included".
var mergeBaseSentences = map[string][]string{
	"POST /api/v1/projects": {
		"Create a project {key, name, default_assignee}: any signed-in person or agent session; a bearer names its session in actor.",
		"Every project names the person its unowned work lands on, the last step of the chain that decides who a question reaches: default_assignee is the email of a person who has signed in, and a call that names none takes the person creating the project, else the person its credential acts for.",
		"A credential that acts for nobody and names no default_assignee is 400 INVALID_PROJECT; a login nobody has signed in with, the one the call names or the one its credential acts for, is 400 ASSIGNEE_NOT_ALLOWED.",
		"A key that is not a project key, an empty name, a name holding a control character or a bidi embedding, override or isolate, or a name with no visible character (a letter, number, punctuation or symbol other than the blank fillers U+115F, U+1160, U+3164, U+FFA0 and U+2800, or a code point Go's Unicode tables leave unassigned, such as an emoji newer than them, other than a noncharacter (U+FDD0-U+FDEF and each code point ending in FFFE or FFFF) or a reserved default-ignorable (U+2065, U+FFF0-U+FFF8, U+E0000-U+E0FFF); combining marks, format characters, private-use characters and whitespace don't count) is 400 INVALID_PROJECT; a name over 80 characters is 400 CAP_EXCEEDED; a key that already exists is 409 PROJECT_EXISTS.",
		"The answer is the project with the default_assignee the create stored.",
	},
	"GET /api/v1/whoami": {
		"Who the server takes the caller for: {kind: user, login, scopes} (login is the person's lowercase email) or {kind: agent, owner, service, session, project, project_scope, scopes, cut, sign_in_required} (owner is a personal token's owner or a broker-issued token's person by lowercase email, null for the shared token and for a broker-issued token that names a service; service is a verified service-account token's Kubernetes subject, or the service a broker-issued token naming no person names: the fleet of a machine nobody has signed in on, a read entry's service, the Kubernetes account an exchange entry lists, or a job's identity as its job entry lists it (a pod's service account as system:serviceaccount:<namespace>:<name>, a workflow as <repository>/<workflow path>@<ref>, or a Google account), null for every other credential; session is the session the caller's credential proved, null for a credential that proves none, which every one but an access token the secrets broker issued is; project is the project key that credential's token names, null where it names none: a job's token whose entry names one project answers that project, and a job's token whose entry names public, which names no one project, answers null; a token that names a project writes other projects' objects as a credential that names none does, a private project it does not name excepted; project_scope is the project scope of an access token the secrets broker issued, left out for every other credential: named for a token an operator's or a job's entry mapped to its project, and for an exchange entry's token naming its account and no project, public for a job's token that names no one project and so writes wherever a token that names no project may, person for a person's own token and for a machine's token that names no person, and none for a read entry's token, while a token narrowed to reading answers the word it narrows; cut is the past time a read-only credential reads at, an RFC 3339 time in UTC, null for a credential with no cut; sign_in_required is true where Dispatch refuses every write of the credential with 403 SIGN_IN_REQUIRED until a person signs in on its machine, which is a broker-issued token of a machine nobody has signed in on, under the project scope person, and false for every other credential, a job's token, an exchange entry's token naming its account and every read-only credential among them). scopes is the OAuth scopes the credential holds, an empty array for one that holds none: person acts as the person on every route the dashboard reaches, and approve settles what a person settles; a read-only credential, which may call only the GET routes Dispatch classes as reads, is answered read after them. A read-only token that carries a cut is answered here, and refused 403 AS_OF_TOKEN_NOT_SERVED on every other read. A Legion daemon's own broker token answers [\"legion-status\"]: with DISPATCH_LEGION_STATUS_SCOPE on, legion-status or person lets a credential make the three writes of a Legion issue that PATCH /api/v1/issues/{key} and POST /api/v1/issues/{key}/status-change refuse to every other credential, but for the owning architect's status write: a broker-issued token whose session the daemon's own token published as the architect of an issue above writes done, backlog, icebox or triage. A write records its actor with proven: true only where this credential proved that actor, the person at a browser or the session above, and never for a session a request names under a credential that proves none.",
	},
}

// TestExistingRouteDescriptionsKeepEverySentenceOfTheMergeBase (R9, written by the A1 tester):
// every sentence a row's description held at the merge base is still in it, whole and unchanged,
// so the head's description differs from the merge base's only by added sentences. At
// 5944573316 POST /api/v1/projects rewrote its first sentence ("any signed-in person" became "any
// signed-in full member ...; a restricted member is 403 RESTRICTED_MEMBER"), which the check
// fails as a changed sentence; the refusal is documented by adding a sentence instead.
func TestExistingRouteDescriptionsKeepEverySentenceOfTheMergeBase(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1: %d %s", response.Code, response.Body.String())
	}
	var index struct {
		Routes []struct {
			Method      string `json:"method"`
			Path        string `json:"path"`
			Description string `json:"description"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &index); err != nil {
		t.Fatalf("decode GET /api/v1: %v", err)
	}
	descriptions := map[string]string{}
	for _, route := range index.Routes {
		descriptions[route.Method+" "+route.Path] = route.Description
	}
	for route, sentences := range mergeBaseSentences {
		description, ok := descriptions[route]
		if !ok {
			t.Errorf("%s is not in the route index", route)
			continue
		}
		for _, sentence := range sentences {
			if !strings.Contains(description, sentence) {
				t.Errorf("%s no longer carries its merge-base sentence %q; c-blob-unchanged allows only added sentences. Its description now reads:\n%s",
					route, sentence, description)
			}
		}
	}
}
