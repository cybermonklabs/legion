package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestForeignChildRefusalHidesPrivateKeysFromViewerWithoutProject (plan D10, R3): the refusals
// that read a parent's open children of other projects, and the results route's filter of a
// check's issues, keep a key only where the request's viewer sees its project, whatever the
// credential lists; the refusal itself still reads every open child (store.AsOwner, plan D5), so
// it fires when only children the caller does not see are open. HUSH is private and owned by
// alice; OPEN is public. A credential that names no project is the case main got wrong: it was
// answered every child (seenForeignChildren, seenConflictIssues and resolveCheck returned every
// row for a credential naming none).
func TestForeignChildRefusalHidesPrivateKeysFromViewerWithoutProject(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedRLSPeople(t, database)
	broker := ciphertokentest.NewStubBroker(t)
	caller := newBrokerCaller(t)
	server := newRLSServer(t, database, rlsServerConfig{broker: broker})
	ruled := newRLSServer(t, database, rlsServerConfig{parentStatusRule: true})
	for _, project := range []string{rlsOpen, rlsHush} {
		server.asOK(t, rlsOwner, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": "The " + project + " project"}, http.StatusCreated)
	}
	file := func(project, title string, fields map[string]any) string {
		body := map[string]any{"project": project, "title": title, "force": true}
		for name, value := range fields {
			body[name] = value
		}
		return createdIssue(t, "file "+title, server.as(t, rlsOwner, http.MethodPost, "/api/v1/issues", body)).Key
	}
	// family is an open parent of OPEN and its open child of HUSH.
	type family struct{ parent, child string }
	newFamily := func(title string) family {
		parent := file(rlsOpen, title+", the parent", nil)
		return family{parent: parent, child: file(rlsHush, title+", the hidden child", map[string]any{"parent": parent})}
	}
	accepted, conflicted, lonely := newFamily("Accepted over a hidden child"), newFamily("Closed over a hidden child"), newFamily("Accepted with only a hidden child open")
	// The results case: a parent of HUSH whose Verification document holds the check k1, and two
	// issues of OPEN under it, the second holding k1 in a Verification document of its own.
	checked := file(rlsHush, "A hidden parent holding the check k1", map[string]any{"spec": "# Spec"})
	uploadDocument(t, server.handler, "/api/v1/issues/"+checked, "verification.md", twoChecks)
	ambiguous := file(rlsOpen, "Holds a check of the hidden parent's id", map[string]any{"parent": checked})
	uploadDocument(t, server.handler, "/api/v1/issues/"+ambiguous, "verification.md", twoChecks)
	makeRLSPrivate(t, database, server.deps.Events, rlsHush)

	acceptance := map[string]any{"status": "done", "refuse_open_foreign_children": true}
	// listedChildren is the keys a 409 FOREIGN_CHILD_OPEN or PARENT_STATUS_CONFLICT lists.
	listedChildren := func(t *testing.T, response *httptest.ResponseRecorder, code string) []string {
		t.Helper()
		refusal := decodeBody[foreignChildOpenAnswer](t, response)
		if response.Code != http.StatusConflict || refusal.Code != code {
			t.Fatalf("status=%d body=%s, want 409 %s", response.Code, response.Body.String(), code)
		}
		keys := []string{}
		for _, issue := range refusal.Issues {
			keys = append(keys, issue.Key)
		}
		return keys
	}
	statusOf := func(t *testing.T, key string) string {
		t.Helper()
		var status string
		if err := database.Pool.QueryRow(context.Background(), `select status from issues where key = $1`, key).Scan(&status); err != nil {
			t.Fatalf("read %s's status: %v", key, err)
		}
		return status
	}
	personal := personalTokenOwnedBy(t, database, rlsOwner, model.ScopePerson)
	brokerToken := "an-alice-token-naming-no-project"
	broker.Says(brokerToken, personScopedTokenAnswer(caller.jkt))

	t.Run("foreign_child_open_lists_no_hidden_key", func(t *testing.T) {
		for _, write := range []struct {
			who  string
			send func(t *testing.T) *httptest.ResponseRecorder
		}{
			{"the sign-in of bob, a full member HUSH does not list", func(t *testing.T) *httptest.ResponseRecorder {
				return server.as(t, rlsMember, http.MethodPatch, "/api/v1/issues/"+accepted.parent, acceptance)
			}},
			{"alice's personal token, whose list names no project", func(t *testing.T) *httptest.ResponseRecorder {
				return server.bearer(t, personal, http.MethodPatch, "/api/v1/issues/"+accepted.parent,
					withMembers(acceptance, map[string]any{"actor": map[string]any{"kind": "session", "id": "ses_personal00001"}}))
			}},
			{"the deployment's shared token", func(t *testing.T) *httptest.ResponseRecorder {
				return server.bearer(t, "agent-token", http.MethodPatch, "/api/v1/issues/"+accepted.parent,
					withMembers(acceptance, map[string]any{"actor": map[string]any{"kind": "session", "id": "ses_sharedtoken01"}}))
			}},
			{"alice's broker token naming no project", func(t *testing.T) *httptest.ResponseRecorder {
				return server.serve(t, caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+accepted.parent, brokerToken, acceptance))
			}},
		} {
			response := write.send(t)
			if listed := listedChildren(t, response, "FOREIGN_CHILD_OPEN"); len(listed) != 0 {
				t.Errorf("%s: the refusal lists %v, want no child of HUSH", write.who, listed)
			}
			if strings.Contains(response.Body.String(), accepted.child) {
				t.Errorf("%s: the refusal names the hidden child %s: %s", write.who, accepted.child, response.Body.String())
			}
		}
	})
	t.Run("a_member_who_sees_the_child_gets_its_key", func(t *testing.T) {
		response := server.as(t, rlsOwner, http.MethodPatch, "/api/v1/issues/"+accepted.parent, acceptance)
		if listed := listedChildren(t, response, "FOREIGN_CHILD_OPEN"); !slices.Equal(listed, []string{accepted.child}) {
			t.Errorf("alice, HUSH's owner, is listed %v, want %s", listed, accepted.child)
		}
	})
	t.Run("parent_status_conflict_lists_no_hidden_key", func(t *testing.T) {
		done := map[string]any{"status": "done"}
		response := ruled.as(t, rlsMember, http.MethodPatch, "/api/v1/issues/"+conflicted.parent, done)
		if listed := listedChildren(t, response, "PARENT_STATUS_CONFLICT"); len(listed) != 0 || strings.Contains(response.Body.String(), conflicted.child) {
			t.Errorf("bob's close of a parent over a hidden child lists %v: %s, want no child of HUSH", listed, response.Body.String())
		}
		if listed := listedChildren(t, ruled.as(t, rlsOwner, http.MethodPatch, "/api/v1/issues/"+conflicted.parent, done), "PARENT_STATUS_CONFLICT"); !slices.Equal(listed, []string{conflicted.child}) {
			t.Errorf("alice's close of the parent lists %v, want %s", listed, conflicted.child)
		}
	})
	t.Run("result_check_filter_lists_no_hidden_check", func(t *testing.T) {
		record := func(login string) *httptest.ResponseRecorder {
			return server.as(t, login, http.MethodPost, "/api/v1/issues/"+ambiguous+"/results", resultBody("k1", "pass", newHeadCommit, nil))
		}
		taken := record(rlsMember)
		if taken.Code != http.StatusCreated {
			t.Fatalf("bob's result against k1: status=%d body=%s, want 201 on %s's own check", taken.Code, taken.Body.String(), ambiguous)
		}
		if strings.Contains(taken.Body.String(), checked) {
			t.Errorf("bob's result names the hidden issue %s: %s", checked, taken.Body.String())
		}
		if check := decodeBody[recordedResult](t, taken).Result.CheckIssueKey; check != ambiguous {
			t.Errorf("bob's result is on the check of %s, want %s's own", check, ambiguous)
		}
		refused := record(rlsOwner)
		if code, message := refusalOf(t, refused); refused.Code != http.StatusConflict || code != "CHECK_AMBIGUOUS" ||
			!strings.Contains(message, checked) || !strings.Contains(message, ambiguous) {
			t.Errorf("alice's result against k1: status=%d body=%s, want 409 CHECK_AMBIGUOUS naming %s and %s", refused.Code, refused.Body.String(), checked, ambiguous)
		}
	})
	t.Run("refusal_fires_when_only_hidden_children_are_open", func(t *testing.T) {
		before := statusOf(t, lonely.parent)
		listedChildren(t, server.as(t, rlsMember, http.MethodPatch, "/api/v1/issues/"+lonely.parent, acceptance), "FOREIGN_CHILD_OPEN")
		if after := statusOf(t, lonely.parent); after != before {
			t.Errorf("the refused acceptance moved %s from %s to %s", lonely.parent, before, after)
		}
	})
}
