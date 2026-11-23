package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// issueReads is every read of one issue the api package serves, by its target with {key} the
// issue's key, {project} its project's and {handoff} the id of a handoff of it: the issue read
// and its summary, the key's resolution, the collaborators, the coverage, the decisions, the
// results, the handoffs three ways, the subscribers, the reference closure (whose loader is the
// issue read's own), and the listing narrowed to the issue's project, unpaged and paged.
var issueReads = []string{
	"/api/v1/issues/{key}",
	"/api/v1/issues/{key}/summary",
	"/api/v1/issues/resolve?ref={key}",
	"/api/v1/issues/{key}/collaborators",
	"/api/v1/issues/{key}/coverage",
	"/api/v1/issues/{key}/decisions",
	"/api/v1/issues/{key}/results",
	"/api/v1/issues/{key}/handoffs",
	"/api/v1/issues/{key}/handoffs/plan/latest",
	"/api/v1/handoffs/{handoff}",
	"/api/v1/issues/{key}/subscribers",
	"/api/v1/issues/{key}/references",
	"/api/v1/issues?project={project}",
	"/api/v1/issues?project={project}&limit=5",
}

// TestAnIssueOfAPrivateProjectAnswersAsMissing: every read of an issue answers a caller that does
// not see the issue's project exactly as it answers a key no issue holds, status and bytes, the
// key itself aside, and answers a caller that sees it the issue. Each caller of verification v16
// reads the issue of each project of the seed through every read issueReads names, and the
// listing of every issue holds exactly the issues of the projects the caller sees.
//
// Each issue carries what makes a read of it differ from a read of a key nobody holds: a handoff,
// a decisions.md, and a session subscribed to the issue's own topic beside one subscribed to every
// topic, on a stand-in listener.
func TestAnIssueOfAPrivateProjectAnswersAsMissing(t *testing.T) {
	t.Parallel()
	// The listener names each issue's key before the seed creates it: a project's first issue is
	// its number 1.
	listener := issueSubscribersListener(t, seedPublic+"-1", seedPrivate+"-1")
	seed := seedPrivateProjectWith(t, testServerOptions{envoyURL: listener.URL})
	if listener.keys != [2]string{seed.issue[seedPublic], seed.issue[seedPrivate]} {
		t.Fatalf("the seed numbered its issues %v, the listener names %v", seed.issue, listener.keys)
	}
	handoffs := map[string]string{}
	for _, project := range []string{seedPublic, seedPrivate} {
		key := seed.issue[project]
		// The issue's session writes its handoff under a credential that proves it, which makes it
		// one of the project's agents, so a viewer who sees the project reads it among the
		// subscribers.
		recorded := seed.asSession(t, issueSubscriberSession(project), project, http.MethodPost, "/api/v1/issues/"+key+"/handoffs/plan", map[string]any{
			"body": `{"plan":"the issue of ` + project + `"}`,
		}, http.StatusCreated)
		handoffs[project] = decodeBody[struct {
			ID string `json:"id"`
		}](t, recorded).ID
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+key+"/artifacts", map[string]any{
			"name": "decisions.md", "content": "## Keep the issue of " + project + "\n\nChosen for " + project + ".\n",
		}, http.StatusCreated)
	}

	for _, caller := range privateCallers {
		t.Run(string(caller), func(t *testing.T) {
			for _, project := range []string{seedPublic, seedPrivate} {
				sees := slices.Contains(caller.sees(), project)
				present := map[string]string{"{key}": seed.issue[project], "{project}": project, "{handoff}": handoffs[project]}
				absent := map[string]string{"{key}": project + "-999999", "{project}": "NOPE", "{handoff}": uuid.NewString()}
				for _, read := range issueReads {
					got := seed.request(t, caller, http.MethodGet, fillIssueRead(read, present), nil)
					asked := fillIssueRead(read, absent)
					missing := seed.request(t, caller, http.MethodGet, asked, nil)
					want := missingAnswerOf(missing.Body.Bytes(), absent, present)
					same := got.Code == missing.Code && bytes.Equal(got.Body.Bytes(), want)
					// A route that refuses the caller's credential before it reads anything answers a
					// key nobody holds that way too, and so must every key, seen or not.
					refused := missing.Code == http.StatusUnauthorized || missing.Code == http.StatusForbidden
					switch {
					case refused && !same:
						t.Errorf("GET %s: the route refuses %s, as GET %s answers %d %s, so it must refuse it alike for %s; it answered %d %s",
							read, caller, asked, missing.Code, want, project, got.Code, got.Body.String())
					case !refused && !sees && !same:
						t.Errorf("GET %s: %s sees no %s, so it must answer as GET %s does, %d %s; it answered %d %s",
							read, caller, project, asked, missing.Code, want, got.Code, got.Body.String())
					case !refused && sees && (got.Code != http.StatusOK || same):
						t.Errorf("GET %s: %s sees %s, so it must read the issue (200, unlike GET %s's %d %s); it answered %d %s",
							read, caller, project, asked, missing.Code, want, got.Code, got.Body.String())
					}
				}
			}
			listed := seed.mustRequest(t, caller, http.MethodGet, "/api/v1/issues", nil, http.StatusOK)
			keys := []string{}
			for _, issue := range decodeBody[[]struct {
				Key string `json:"key"`
			}](t, listed) {
				keys = append(keys, issue.Key)
			}
			want := []string{}
			for _, project := range caller.sees() {
				want = append(want, seed.issue[project])
			}
			slices.Sort(keys)
			slices.Sort(want)
			if !slices.Equal(keys, want) {
				t.Errorf("GET /api/v1/issues: %s lists %v, want the issues of the projects it sees, %v", caller, keys, want)
			}
		})
	}

	// A create reads its body's project as its creator does, its near-copy gate included: alice's
	// create on PRIV under the title of PRIV's issue answers exactly as the same create on a
	// project no row holds, and does not name PRIV's issue as its near copy. carol's, which sees
	// PRIV, is refused naming it, so the title is one the gate refuses.
	t.Run("a create under the title of an issue its creator does not see", func(t *testing.T) {
		create := func(caller privateCaller, project string) *httptest.ResponseRecorder {
			return seed.request(t, caller, http.MethodPost, "/api/v1/issues", map[string]string{
				"project": project, "title": "The issue of " + seedPrivate,
			})
		}
		got, missing := create(callerAlice, seedPrivate), create(callerAlice, "NOPE")
		want := missingAnswerOf(missing.Body.Bytes(), map[string]string{"{project}": "NOPE"}, map[string]string{"{project}": seedPrivate})
		if got.Code != missing.Code || !bytes.Equal(got.Body.Bytes(), want) {
			t.Errorf("POST /api/v1/issues on %s: alice must be answered as on a project no row holds, %d %s; she was answered %d %s",
				seedPrivate, missing.Code, want, got.Code, got.Body.String())
		}
		refused := create(callerCarol, seedPrivate)
		if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), seed.issue[seedPrivate]) {
			t.Errorf("POST /api/v1/issues on %s: carol, who sees it, must be refused naming %s; she was answered %d %s",
				seedPrivate, seed.issue[seedPrivate], refused.Code, refused.Body.String())
		}
	})
}

// fillIssueRead is read with each placeholder replaced by its value.
func fillIssueRead(read string, values map[string]string) string {
	for placeholder, value := range values {
		read = strings.ReplaceAll(read, placeholder, value)
	}
	return read
}

// missingAnswerOf is the body a read of the absent values answered, with each of them written as
// the present value it stands for: the bytes a read of the present values must answer when it
// answers as missing, since an answer that names what it was asked for names it either way.
func missingAnswerOf(body []byte, absent, present map[string]string) []byte {
	for placeholder, value := range absent {
		body = bytes.ReplaceAll(body, []byte(value), []byte(present[placeholder]))
	}
	return body
}

// issueSubscriberSession is the session subscribed to the issue of project alone, which also
// writes that issue's handoff, so it has acted in the project.
func issueSubscriberSession(project string) string {
	return "s-" + strings.ToLower(project)
}

// subscribersListener is a stand-in Envoy listener for the subscriber reads: one session
// subscribed to every Dispatch topic and one subscribed to each issue's own topic, none of them
// registered, and an empty Legion agent registry.
type subscribersListener struct {
	*httptest.Server
	keys [2]string
}

// issueSubscribersListener starts the stand-in for the public issue's key and the private one's.
func issueSubscribersListener(t *testing.T, public, private string) subscribersListener {
	t.Helper()
	interests, err := json.Marshal([]map[string]any{
		{"session_id": "s-wide", "topics": []string{"notifications.dispatch.>"}, "updated_at": 3},
		{"session_id": issueSubscriberSession(seedPublic), "topics": []string{issueSubscriberBase(public) + ".>"}, "updated_at": 2},
		{"session_id": issueSubscriberSession(seedPrivate), "topics": []string{issueSubscriberBase(private) + ".>"}, "updated_at": 1},
	})
	if err != nil {
		t.Fatalf("encode the listener's interests: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/interests/":
			_, _ = w.Write(interests)
		case "/v1/sessions", "/v1/legion/agents":
			_, _ = w.Write([]byte("[]"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return subscribersListener{Server: server, keys: [2]string{public, private}}
}
