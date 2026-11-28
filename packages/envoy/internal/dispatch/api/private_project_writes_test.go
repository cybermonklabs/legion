// private_project_writes_test.go holds the private-project clause of INFRA-4799's R8: a broker
// token that names one project writes no object of a private project that it does not name. Each
// write check that asks access.CredentialCovers has a case of its own. Each HTTP case
// makes the write to the private project's object and the same write to a target that does not
// exist, and holds the two answers equal in status, code and message. Each reads what the two
// writes could add before and after them, and finds nothing written. The same write with a token
// that names the private project is taken.
//
// Some checks run behind an earlier check that refuses the same key with the same answer. The
// HTTP case of such a check holds the answer and that nothing is written, and the check's own
// branch decides the case only where the earlier check passes the key:
//
//   - foreignDependencyTargets refuses a parent and a blocked_by target before the reads under
//     the locks. TestTheReadsUnderTheLocksAnswerAPrivateKeyAsAMissingKey calls each of those
//     reads directly, readCreateParent, validateIssueParent and writeIssueNeeds, and so holds the
//     branch of each.
//   - resolveCheck leaves out a check of a private project before readResultRequest asks about
//     the check's issue (refuseUncoveredCheck). The project check of a retraction asks about the
//     check's issue before the keyed layer and the handler (secondProjectResolverFor), and
//     retractCheckResult asks again in its transaction (refuseRetractionOfUncoveredCheck).
//     TestTheResultRoutesSecondChecksAnswerAPrivateCheckAsAMissingOne calls each of the two
//     second checks directly, and so holds the branch of each.
//   - withinProjectScope refuses a status change of a private project's issue before the
//     handler runs, and planStatusChangeUnderLocks asks about the issue the call names again
//     under its owner locks. TestAStatusChangeWhoseIssueTurnedPrivateAnswersAsAMissingOne
//     (private_project_cascade_test.go) calls the route's handler past the pre-handler check,
//     and so holds the branch of the second.
//
// A status change leaves the private issue out of its cascade, and the answers 409
// PARENT_STATUS_CONFLICT and 409 FOREIGN_CHILD_OPEN leave it out of their issues.
package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const (
	// privateTestProject is the project each test makes private.
	privateTestProject = "PRIV"
	// scopedToken names brokerTestProject alone, so it does not name privateTestProject.
	scopedToken = "a-token-naming-its-own-project"
	// namingToken names privateTestProject alone.
	namingToken = "a-token-naming-the-private-project"
	// missingIssue is an issue key that no issue holds.
	missingIssue = "NONE-1"
	// missingProject is a project key that no project holds.
	missingProject = "NONE"
	// missingResult is a result id that no result holds.
	missingResult = "5d4a3b2c-1e0f-4a9b-8c7d-6e5f4a3b2c1d"
)

// privateProjectRig is the broker-token rig with the projects brokerTestProject and
// privateTestProject, both public until makePrivate, and the two broker tokens scopedToken and
// namingToken, both of one key and one session.
type privateProjectRig struct {
	*brokerTokenRig
	caller *brokerCaller
}

// newPrivateProjectRig builds the rig on a server built from serving.
func newPrivateProjectRig(t *testing.T, serving testServerOptions) *privateProjectRig {
	t.Helper()
	rig := &privateProjectRig{brokerTokenRig: newBrokerTokenRigOn(t, serving), caller: newBrokerCaller(t)}
	for token, project := range map[string]string{scopedToken: brokerTestProject, namingToken: privateTestProject} {
		answer := liveNamedToken(rig.caller.jkt)
		answer.Projects = []string{project}
		rig.broker.Says(token, answer)
	}
	for _, project := range []string{brokerTestProject, privateTestProject} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": project}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, created.Code, created.Body.String())
		}
	}
	return rig
}

// issue is the key of an issue a person files in project with title and fields.
func (rig *privateProjectRig) issue(t *testing.T, project, title string, fields map[string]any) string {
	t.Helper()
	body := map[string]any{"project": project, "title": title, "force": true}
	for name, value := range fields {
		body[name] = value
	}
	return createdIssue(t, "file "+title, rig.asPerson(t, http.MethodPost, "/api/v1/issues", body)).Key
}

// makePrivate makes privateTestProject private, with the test's person as its owner, in one
// transaction of the rig's database.
func (rig *privateProjectRig) makePrivate(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the visibility write: %v", err)
	}
	defer tx.Rollback(ctx)
	by := model.Actor{Kind: "user", ID: brokerTestPerson}
	if _, _, err := access.SetOwner(ctx, tx, rig.deps.Events, privateTestProject, brokerTestPerson, by); err != nil {
		t.Fatalf("give %s to %s: %v", privateTestProject, brokerTestPerson, err)
	}
	if _, _, err := access.SetVisibility(ctx, tx, rig.deps.Events, privateTestProject, model.ProjectPrivate, by); err != nil {
		t.Fatalf("make %s private: %v", privateTestProject, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the visibility write: %v", err)
	}
}

// send is the request of the rig's caller under token.
func (rig *privateProjectRig) send(t *testing.T, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return rig.serve(rig.caller.presenting(t, method, path, token, body))
}

// written is what a refused write must leave as it was: the status, the parent and the last_seq
// of each issue of keys, the rows of issues, issue_links, check_results, check_result_retractions,
// going_requests and near_copy_checks, and the events, comments and asks of each issue of keys.
func (rig *privateProjectRig) written(t *testing.T, keys ...string) string {
	t.Helper()
	var held string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select coalesce((select string_agg(key || ' ' || status || ' ' || coalesce(parent_key, '-') || ' ' || last_seq, ', ' order by key)
		                 from issues where key = any($1)), '')
		       || '; issues ' || (select count(*) from issues)
		       || '; issue_links ' || (select count(*) from issue_links)
		       || '; check_results ' || (select count(*) from check_results)
		       || '; check_result_retractions ' || (select count(*) from check_result_retractions)
		       || '; going_requests ' || (select count(*) from going_requests)
		       || '; near_copy_checks ' || (select count(*) from near_copy_checks)
		       || '; events ' || (select count(*) from events where issue_key = any($1))
		       || '; comments ' || (select count(*) from comments where issue_key = any($1))
		       || '; asks ' || (select count(*) from asks where issue_key = any($1))
	`, keys).Scan(&held); err != nil {
		t.Fatalf("read what the write could add: %v", err)
	}
	return held
}

// refusedAsMissing fails t unless private, the write to the private project's target, answers
// status and code, and answers exactly what missing, the same write to a target that does not
// exist, answers, once missingTarget is read as privateTarget in missing's body. Neither write may
// change what written reads of keys.
func (rig *privateProjectRig) refusedAsMissing(t *testing.T, keys []string, status int, code, privateTarget, missingTarget string, private, missing func() *httptest.ResponseRecorder) {
	t.Helper()
	before := rig.written(t, keys...)
	refused := private()
	if after := rig.written(t, keys...); after != before {
		t.Errorf("the refused write changed %s to %s, want nothing written", before, after)
	}
	if got, _ := refusalOf(t, refused); refused.Code != status || got != code {
		t.Fatalf("the write to the private project: status=%d body=%s, want %d %s", refused.Code, refused.Body.String(), status, code)
	}
	absent := missing()
	if after := rig.written(t, keys...); after != before {
		t.Errorf("the write to a target that does not exist changed %s to %s, want nothing written", before, after)
	}
	want := strings.ReplaceAll(absent.Body.String(), missingTarget, privateTarget)
	if refused.Code != absent.Code || refused.Body.String() != want {
		t.Errorf("the write to the private project answers %d %s, and the write to a target that does not exist answers %d %s, want one answer",
			refused.Code, refused.Body.String(), absent.Code, absent.Body.String())
	}
}

// requireTaken fails t unless response answers status.
func requireTaken(t *testing.T, what string, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("%s: status=%d body=%s, want %d", what, response.Code, response.Body.String(), status)
	}
}

// TestAPrivateProjectsObjectAnswersAsAMissingObjectToAnotherProjectsToken: a token that names its
// own project writes no object of a private project that it does not name, and gets the answer of
// an object that does not exist. Three checks read the private project from the address or the
// body before the handler writes anything:
//
//   - withinProjectScope, for a comment on the private project's issue: 403 PROJECT_SCOPE, the
//     answer for an issue key that no issue holds.
//   - createIssue's check of the body's project, for an issue filed into the private project: 404
//     NOT_FOUND, the answer for a project that does not exist. The check runs before the body's
//     other fields are judged, so a create with no title gets the same 404 for both projects.
//   - createGoingRequest's check of the body's project, for a question of the private project: 404
//     PROJECT_NOT_FOUND, the answer for a project that does not exist, which a person still asks
//     of as before.
//
// The same write with the token that names the private project is taken.
func TestAPrivateProjectsObjectAnswersAsAMissingObjectToAnotherProjectsToken(t *testing.T) {
	t.Parallel()
	rig := newPrivateProjectRig(t, testServerOptions{})
	private := rig.issue(t, privateTestProject, "A private project's issue", nil)
	rig.makePrivate(t)

	t.Run("withinProjectScope: a comment on the private project's issue", func(t *testing.T) {
		comment := func(token, issue string) func() *httptest.ResponseRecorder {
			return func() *httptest.ResponseRecorder {
				return rig.send(t, token, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{"body": "A remark."})
			}
		}
		rig.refusedAsMissing(t, []string{private}, http.StatusForbidden, codeProjectScope, private, missingIssue,
			comment(scopedToken, private), comment(scopedToken, missingIssue))
		requireTaken(t, "the comment of the token that names the private project", comment(namingToken, private)(), http.StatusCreated)
	})

	t.Run("createIssue: an issue filed into the private project", func(t *testing.T) {
		create := func(token, project, title string) func() *httptest.ResponseRecorder {
			return func() *httptest.ResponseRecorder {
				return rig.send(t, token, http.MethodPost, "/api/v1/issues", map[string]any{
					"project": project, "title": title, "spec": "# Spec", "force": true,
				})
			}
		}
		for _, title := range []string{"Filed into another project", ""} {
			rig.refusedAsMissing(t, []string{private}, http.StatusNotFound, "NOT_FOUND", privateTestProject, missingProject,
				create(scopedToken, privateTestProject, title), create(scopedToken, missingProject, title))
		}
		created := create(namingToken, privateTestProject, "Filed into another project")()
		if issue := createdIssue(t, "the create of the token that names the private project", created); issue.Project != privateTestProject {
			t.Fatalf("the create made %s of project %s, want an issue of %s", issue.Key, issue.Project, privateTestProject)
		}
	})

	t.Run("createGoingRequest: a question of the private project", func(t *testing.T) {
		ask := func(token, project string) func() *httptest.ResponseRecorder {
			return func() *httptest.ResponseRecorder {
				return rig.send(t, token, http.MethodPost, "/api/v1/going/requests", map[string]any{
					"words": "What is going on there?", "project": project,
				})
			}
		}
		rig.refusedAsMissing(t, []string{private}, http.StatusNotFound, "PROJECT_NOT_FOUND", privateTestProject, missingProject,
			ask(scopedToken, privateTestProject), ask(scopedToken, missingProject))
		requireTaken(t, "the question of the token that names the private project", ask(namingToken, privateTestProject)(), http.StatusCreated)
		// A credential that names no project is answered as before: its question of a project
		// that does not exist is recorded.
		requireTaken(t, "a person's question of a project that does not exist", rig.asPerson(t, http.MethodPost, "/api/v1/going/requests", map[string]any{
			"words": "What is going on there?", "project": missingProject,
		}), http.StatusCreated)
	})
}

// TestAPrivateParentOrBlockedByTargetAnswersAsAMissingIssueToAnotherProjectsToken: a token that
// names its own project links no blocked_by target, and names no parent, of a private project that
// it does not name. It gets the answer of a key that no issue holds: 400 BLOCKED_BY_INPUT for a
// target and 400 PARENT_INPUT for a parent. foreignDependencyTargets refuses both keys first, on
// the pool, before the near-copy gate of a create and before a close settles the issue's
// documents, so both get one answer whatever else the body holds. Each case holds that answer and
// that neither write writes anything:
//
//   - foreignDependencyTargets, for a blocked_by target and for a parent: a close that also sets the
//     field. The issue's spec holds a decision block, whose settlement opens an ask. So a check
//     that let either key through would leave that ask and its events, even where the write under
//     the locks refuses the key after it. The same close with a stale expected_sequence, which the
//     write under the locks would refuse 409 ISSUE_CHANGED, gets the same 400 for both keys.
//   - A create that names the parent, a PATCH that sets the parent, and a create that names the
//     target, the writes whose reads under the locks are readCreateParent, validateIssueParent and
//     writeIssueNeeds. foreignDependencyTargets refuses each key first, so these cases hold the
//     answer and that nothing is written. The create's cases also send no title, which the create
//     judges after foreignDependencyTargets.
//
// The same write with the token that names the private project is taken.
func TestAPrivateParentOrBlockedByTargetAnswersAsAMissingIssueToAnotherProjectsToken(t *testing.T) {
	t.Parallel()
	rig := newPrivateProjectRig(t, testServerOptions{settle: time.Hour})
	private := rig.issue(t, privateTestProject, "A private project's issue", map[string]any{"spec": "# Spec"})
	// closer files an issue of the token's own project whose spec's decision block a close settles.
	closer := func(title string) string {
		return rig.issue(t, brokerTestProject, title, map[string]any{"spec": closeDecision})
	}
	closers := map[string][3]string{}
	for _, field := range []string{"blocked_by", "parent"} {
		closers[field] = [3]string{closer("Closed with a private " + field), closer("Closed with a missing " + field), closer("Closed by the naming token with a " + field)}
	}
	parents := [3]string{
		rig.issue(t, brokerTestProject, "Put under a private parent", nil),
		rig.issue(t, brokerTestProject, "Put under a missing parent", nil),
		rig.issue(t, brokerTestProject, "Put under a parent by the naming token", nil),
	}
	rig.makePrivate(t)
	// value is what a write sends in field to name key.
	value := func(field, key string) any {
		if field == "blocked_by" {
			return []string{key}
		}
		return key
	}
	notFound := map[string]string{"blocked_by": "BLOCKED_BY_INPUT", "parent": "PARENT_INPUT"}

	for _, field := range []string{"blocked_by", "parent"} {
		t.Run("foreignDependencyTargets: a close that sets "+field, func(t *testing.T) {
			close := func(token, issue, target string, members map[string]any) func() *httptest.ResponseRecorder {
				return func() *httptest.ResponseRecorder {
					return rig.send(t, token, http.MethodPatch, "/api/v1/issues/"+issue,
						withMembers(map[string]any{"status": "done", field: value(field, target)}, members))
				}
			}
			issues := closers[field]
			for _, members := range []map[string]any{nil, {"expected_sequence": 0}} {
				rig.refusedAsMissing(t, []string{issues[0], issues[1], private}, http.StatusBadRequest, notFound[field], private, missingIssue,
					close(scopedToken, issues[0], private, members), close(scopedToken, issues[1], missingIssue, members))
			}
			closed := close(namingToken, issues[2], private, nil)()
			requireTaken(t, "the close of the token that names the private project", closed, http.StatusOK)
			if issue := decodeBody[model.Issue](t, closed); issue.Status != "done" {
				t.Fatalf("the close left %s %s, want done", issue.Key, issue.Status)
			}
		})
	}

	// readCreateParent, the create's read of its parent under the locks, is reached only where
	// foreignDependencyTargets passes the key. This HTTP case holds the answer and that nothing is
	// written, and TestTheReadsUnderTheLocksAnswerAPrivateKeyAsAMissingKey holds
	// readCreateParent's own branch.
	t.Run("the create's read of its parent: a create under the private issue", func(t *testing.T) {
		create := func(token, parent, title string) func() *httptest.ResponseRecorder {
			return func() *httptest.ResponseRecorder {
				return rig.send(t, token, http.MethodPost, "/api/v1/issues", map[string]any{
					"project": brokerTestProject, "title": title, "parent": parent, "force": true,
				})
			}
		}
		for _, title := range []string{"Filed under another project's issue", ""} {
			rig.refusedAsMissing(t, []string{private}, http.StatusBadRequest, "PARENT_INPUT", private, missingIssue,
				create(scopedToken, private, title), create(scopedToken, missingIssue, title))
		}
		if issue := createdIssue(t, "the create of the token that names the private project", create(namingToken, private, "Filed under another project's issue")()); issue.Parent == nil || *issue.Parent != private {
			t.Fatalf("the create made %s under %v, want it under %s", issue.Key, issue.Parent, private)
		}
	})

	// validateIssueParent is reached only where foreignDependencyTargets passes the key. This
	// HTTP case holds the answer and that nothing is written, and
	// TestTheReadsUnderTheLocksAnswerAPrivateKeyAsAMissingKey holds validateIssueParent's own
	// branch.
	t.Run("validateIssueParent: a PATCH that puts an issue under the private issue", func(t *testing.T) {
		reparent := func(token, issue, parent string) func() *httptest.ResponseRecorder {
			return func() *httptest.ResponseRecorder {
				return rig.send(t, token, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"parent": parent})
			}
		}
		rig.refusedAsMissing(t, []string{parents[0], private}, http.StatusBadRequest, "PARENT_INPUT", private, missingIssue,
			reparent(scopedToken, parents[0], private), reparent(scopedToken, parents[1], missingIssue))
		moved := reparent(namingToken, parents[2], private)()
		requireTaken(t, "the PATCH of the token that names the private project", moved, http.StatusOK)
		if issue := decodeBody[model.Issue](t, moved); issue.Parent == nil || *issue.Parent != private {
			t.Fatalf("the PATCH left %s under %v, want it under %s", issue.Key, issue.Parent, private)
		}
	})

	// writeIssueNeeds is reached only where foreignDependencyTargets passes the key. This HTTP case
	// holds the answer and that nothing is written, and
	// TestTheReadsUnderTheLocksAnswerAPrivateKeyAsAMissingKey holds writeIssueNeeds's own branch.
	t.Run("writeIssueNeeds: a create blocked by the private issue", func(t *testing.T) {
		create := func(token, target, title string) func() *httptest.ResponseRecorder {
			return func() *httptest.ResponseRecorder {
				return rig.send(t, token, http.MethodPost, "/api/v1/issues", map[string]any{
					"project": brokerTestProject, "title": title, "blocked_by": []string{target}, "force": true,
				})
			}
		}
		for _, title := range []string{"Waits on another project's issue", ""} {
			rig.refusedAsMissing(t, []string{private}, http.StatusBadRequest, "BLOCKED_BY_INPUT", private, missingIssue,
				create(scopedToken, private, title), create(scopedToken, missingIssue, title))
		}
		if issue := createdIssue(t, "the create of the token that names the private project", create(namingToken, private, "Waits on another project's issue")()); !slices.Equal(issue.BlockedBy, []string{private}) {
			t.Fatalf("the create made %s blocked by %v, want %s", issue.Key, issue.BlockedBy, private)
		}
	})
}

// TestTheReadsUnderTheLocksAnswerAPrivateKeyAsAMissingKey: readCreateParent, validateIssueParent
// and writeIssueNeeds, the reads under the locks of a create's parent, of a PATCH's parent and of a
// wait's target (a blocked_by target here), each answer a parent or a target of a private project
// that the caller does not name with the error of a key that no issue holds, parentNotFound and
// waitNotFound, in
// status, code and message. Through HTTP, foreignDependencyTargets refuses such a key before each
// read, so this test calls each one directly, in a transaction of the test database that it rolls
// back, and so holds the branch of each. With a caller that names the private project, each takes
// the key.
func TestTheReadsUnderTheLocksAnswerAPrivateKeyAsAMissingKey(t *testing.T) {
	t.Parallel()
	rig := newPrivateProjectRig(t, testServerOptions{})
	private := rig.issue(t, privateTestProject, "A private project's issue", nil)
	own := rig.issue(t, brokerTestProject, "An issue of the caller's own project", nil)
	rig.makePrivate(t)
	ctx := context.Background()
	for _, site := range []struct {
		name, code string
		// read runs the site for a caller that names the project caller, with key as the parent
		// or the blocked_by target of the issue own, or as the parent of the issue a create files.
		read func(tx pgx.Tx, caller, key string) error
	}{
		{"readCreateParent", "PARENT_INPUT", func(tx pgx.Tx, caller, key string) error {
			_, _, err := readCreateParent(ctx, tx, caller, key)
			return err
		}},
		{"validateIssueParent", "PARENT_INPUT", func(tx pgx.Tx, caller, key string) error {
			return validateIssueParent(ctx, tx, own, caller, key)
		}},
		{"writeIssueNeeds", "BLOCKED_BY_INPUT", func(tx pgx.Tx, caller, key string) error {
			waits, _, err := resolveIssueWaits(nil, &[]string{key})
			if err != nil {
				return err
			}
			_, err = writeIssueNeeds(ctx, tx, own, brokerTestProject, caller, waits, heldIssueNeeds{}, time.Now())
			return err
		}},
	} {
		t.Run(site.name, func(t *testing.T) {
			// answer is what the site answers, in a transaction that it rolls back.
			answer := func(caller, key string) error {
				t.Helper()
				tx, err := rig.database.Pool.Begin(ctx)
				if err != nil {
					t.Fatalf("begin the transaction of %s: %v", site.name, err)
				}
				defer tx.Rollback(ctx)
				return site.read(tx, caller, key)
			}
			var refused, missing *apiError
			if err := answer(brokerTestProject, private); !errors.As(err, &refused) {
				t.Fatalf("%s of the private key %s answered %v, want the error of a key that no issue holds", site.name, private, err)
			}
			if err := answer(brokerTestProject, missingIssue); !errors.As(err, &missing) {
				t.Fatalf("%s of the missing key %s answered %v, want its 400 %s", site.name, missingIssue, err, site.code)
			}
			want := strings.ReplaceAll(missing.message, missingIssue, private)
			if refused.status != http.StatusBadRequest || refused.code != site.code ||
				refused.status != missing.status || refused.code != missing.code || refused.message != want {
				t.Errorf("%s answers %d %s %q for the private key and %d %s %q for a missing key, want one 400 %s",
					site.name, refused.status, refused.code, refused.message, missing.status, missing.code, missing.message, site.code)
			}
			if err := answer(privateTestProject, private); err != nil {
				t.Errorf("%s for a caller that names %s answered %v, want the key taken", site.name, privateTestProject, err)
			}
		})
	}
}

// TestTheResultRoutesSecondChecksAnswerAPrivateCheckAsAMissingOne: refuseUncoveredCheck and
// refuseRetractionOfUncoveredCheck, the checks of a check's issue's project that readResultRequest
// and retractCheckResult make after their route's first check, each answer a check of a private
// project that the caller does not name as their route answers a target that does not exist, in
// status, code and message: 400 UNKNOWN_CHECK, a result's answer for a check that no Verification
// document above its issue holds, and 403 PROJECT_SCOPE, a retraction's answer for a result id
// that no result holds. Through HTTP, resolveCheck and withinProjectScope refuse such a check
// before either, so this test calls each one directly, in a transaction of the test database that
// it rolls back, and so holds the branch of each. With a caller that names the private project,
// each takes the check.
func TestTheResultRoutesSecondChecksAnswerAPrivateCheckAsAMissingOne(t *testing.T) {
	t.Parallel()
	rig := newPrivateProjectRig(t, testServerOptions{})
	parent := rig.issue(t, privateTestProject, "A private project's approved issue", map[string]any{"spec": "# Spec"})
	uploadDocument(t, rig.handler, "/api/v1/issues/"+parent, "verification.md", twoChecks)
	own := rig.issue(t, brokerTestProject, "Records on a private project's check", map[string]any{"parent": parent})
	rig.makePrivate(t)
	ctx := context.Background()
	for _, site := range []struct {
		name, code string
		status     int
		// check runs the site for a caller that names the project caller, on the check k1 of the
		// private issue parent.
		check func(tx pgx.Tx, caller string) error
		// missing is the status and the body that the route answers the scoped token for a target
		// that does not exist, as they read for the check k1.
		missing func(t *testing.T) (int, string)
	}{
		{
			name: "refuseUncoveredCheck", code: "UNKNOWN_CHECK", status: http.StatusBadRequest,
			check: func(tx pgx.Tx, caller string) error {
				return refuseUncoveredCheck(ctx, tx, caller, parent, "k1")
			},
			// A result on own against k9, which no Verification document above own holds.
			missing: func(t *testing.T) (int, string) {
				absent := rig.send(t, scopedToken, http.MethodPost, "/api/v1/issues/"+own+"/results", resultBody("k9", "pass", newHeadCommit, nil))
				return absent.Code, strings.ReplaceAll(absent.Body.String(), "k9", "k1")
			},
		},
		{
			name: "refuseRetractionOfUncoveredCheck", code: codeProjectScope, status: http.StatusForbidden,
			check: func(tx pgx.Tx, caller string) error {
				return refuseRetractionOfUncoveredCheck(ctx, tx, caller, parent)
			},
			// A retraction of a result id that no result holds, whose answer names no result.
			missing: func(t *testing.T) (int, string) {
				absent := rig.send(t, scopedToken, http.MethodPost, "/api/v1/results/"+missingResult+"/retract",
					map[string]any{"reason": "The run was against the wrong commit."})
				return absent.Code, absent.Body.String()
			},
		},
	} {
		t.Run(site.name, func(t *testing.T) {
			// answer is what the site answers, in a transaction that it rolls back.
			answer := func(caller string) error {
				t.Helper()
				tx, err := rig.database.Pool.Begin(ctx)
				if err != nil {
					t.Fatalf("begin the transaction of %s: %v", site.name, err)
				}
				defer tx.Rollback(ctx)
				return site.check(tx, caller)
			}
			err := answer(brokerTestProject)
			if err == nil {
				t.Fatalf("%s took the private project's check k1, want the answer of a target that does not exist", site.name)
			}
			refused := httptest.NewRecorder()
			rig.served.writeHandlerError(refused, err)
			if code, _ := refusalOf(t, refused); refused.Code != site.status || code != site.code {
				t.Fatalf("%s of the private project's check k1: status=%d body=%s, want %d %s",
					site.name, refused.Code, refused.Body.String(), site.status, site.code)
			}
			if status, body := site.missing(t); refused.Code != status || refused.Body.String() != body {
				t.Errorf("%s answers %d %s for the private project's check, and its route answers %d %s for a target that does not exist, want one answer",
					site.name, refused.Code, refused.Body.String(), status, body)
			}
			if err := answer(privateTestProject); err != nil {
				t.Errorf("%s for a caller that names %s answered %v, want the check taken", site.name, privateTestProject, err)
			}
		})
	}
}

// TestAResultOnAPrivateProjectsCheckAnswersAsAMissingCheckToAnotherProjectsToken: an issue of the
// token's own project stands under an issue of the private project, whose Verification document
// holds the check k1. The path names the issue of the token's own project, so each write passes
// withinProjectScope and reaches the results route's own check of the check's issue.
//
//   - resolveCheck and readResultRequest: a result recorded against k1, and its dry run, answer
//     400 UNKNOWN_CHECK, the answer for a check that no Verification document above the issue
//     holds.
//   - resolveCheck: a second issue of the token's own project under the private issue holds k1
//     in its own Verification document too. The token's result against k1 and its dry run are
//     taken on that issue's check, and name no issue of the private project. The token that
//     names the private project gets 409 CHECK_AMBIGUOUS, which names both issues.
//   - retractCheckResult: the retraction of a result recorded against k1 before the project was
//     made private answers 403 PROJECT_SCOPE, the answer for a result id that no result holds,
//     also for a body with a blank reason, and also under an Idempotency-Key: the project check
//     asks about the check's issue before the keyed layer, so neither answer echoes the key, a
//     second body under the key is refused as the first was, and a malformed key changes nothing.
//
// resolveCheck and the retraction's project check refuse k1 before the second checks of
// readResultRequest and retractCheckResult, so these cases hold the answer and that nothing is
// written, and TestTheResultRoutesSecondChecksAnswerAPrivateCheckAsAMissingOne holds the branch of
// each second check. The same writes with the token that names the private project are taken.
func TestAResultOnAPrivateProjectsCheckAnswersAsAMissingCheckToAnotherProjectsToken(t *testing.T) {
	t.Parallel()
	rig := newPrivateProjectRig(t, testServerOptions{})
	parent := rig.issue(t, privateTestProject, "A private project's approved issue", map[string]any{"spec": "# Spec"})
	uploadDocument(t, rig.handler, "/api/v1/issues/"+parent, "verification.md", twoChecks)
	own := rig.issue(t, brokerTestProject, "Records on a private project's check", map[string]any{"parent": parent})
	recorded := rig.send(t, scopedToken, http.MethodPost, "/api/v1/issues/"+own+"/results", resultBody("k1", "pass", newHeadCommit, nil))
	requireTaken(t, "the result recorded before the project is private", recorded, http.StatusCreated)
	result := decodeBody[recordedResult](t, recorded).Result.ID
	ambiguous := rig.issue(t, brokerTestProject, "Holds a check of the private parent's id", map[string]any{"parent": parent})
	uploadDocument(t, rig.handler, "/api/v1/issues/"+ambiguous, "verification.md", twoChecks)
	rig.makePrivate(t)

	for _, route := range []struct{ name, path string }{
		{"readResultRequest: a result", "/api/v1/issues/" + own + "/results"},
		{"readResultRequest: a result's dry run", "/api/v1/issues/" + own + "/results/dry-run"},
	} {
		t.Run(route.name+" on the private project's check", func(t *testing.T) {
			record := func(token, check string) func() *httptest.ResponseRecorder {
				return func() *httptest.ResponseRecorder {
					return rig.send(t, token, http.MethodPost, route.path, resultBody(check, "pass", newHeadCommit, nil))
				}
			}
			rig.refusedAsMissing(t, []string{own, parent}, http.StatusBadRequest, "UNKNOWN_CHECK", "k1", "k9",
				record(scopedToken, "k1"), record(scopedToken, "k9"))
			want := http.StatusCreated
			if strings.HasSuffix(route.path, "/dry-run") {
				want = http.StatusOK
			}
			requireTaken(t, "the same write of the token that names the private project", record(namingToken, "k1")(), want)
		})
	}

	t.Run("resolveCheck: a check id that the private parent's Verification document holds too", func(t *testing.T) {
		record := func(token, path string) *httptest.ResponseRecorder {
			return rig.send(t, token, http.MethodPost, "/api/v1/issues/"+ambiguous+path, resultBody("k1", "pass", newHeadCommit, nil))
		}
		for _, route := range []struct {
			path string
			want int
		}{{"/results/dry-run", http.StatusOK}, {"/results", http.StatusCreated}} {
			taken := record(scopedToken, route.path)
			requireTaken(t, "the scoped token's write to "+route.path, taken, route.want)
			if strings.Contains(taken.Body.String(), parent) {
				t.Errorf("the scoped token's write to %s names the private issue %s: %s", route.path, parent, taken.Body.String())
			}
			if route.path == "/results" {
				if check := decodeBody[recordedResult](t, taken).Result.CheckIssueKey; check != ambiguous {
					t.Errorf("the scoped token's result is on the check of %s, want the check of %s", check, ambiguous)
				}
			}
		}
		for _, path := range []string{"/results/dry-run", "/results"} {
			refused := record(namingToken, path)
			if code, message := refusalOf(t, refused); refused.Code != http.StatusConflict || code != "CHECK_AMBIGUOUS" ||
				!strings.Contains(message, parent) || !strings.Contains(message, ambiguous) {
				t.Errorf("the write to %s of the token that names the private project: status=%d body=%s, want 409 CHECK_AMBIGUOUS naming %s and %s",
					path, refused.Code, refused.Body.String(), parent, ambiguous)
			}
		}
	})

	t.Run("retractCheckResult: a retraction of a result on the private project's check", func(t *testing.T) {
		retract := func(token, id, reason string) func() *httptest.ResponseRecorder {
			return func() *httptest.ResponseRecorder {
				return rig.send(t, token, http.MethodPost, "/api/v1/results/"+id+"/retract", map[string]any{"reason": reason})
			}
		}
		for _, reason := range []string{"The run was against the wrong commit.", " "} {
			rig.refusedAsMissing(t, []string{own, parent}, http.StatusForbidden, codeProjectScope, result, missingResult,
				retract(scopedToken, result, reason), retract(scopedToken, missingResult, reason))
		}
		keyed := func(id, key, reason string) *httptest.ResponseRecorder {
			request := rig.caller.presenting(t, http.MethodPost, "/api/v1/results/"+id+"/retract", scopedToken, map[string]any{"reason": reason})
			request.Header.Set("Idempotency-Key", key)
			return rig.serve(request)
		}
		for _, key := range []string{uuid.NewString(), "not a key"} {
			for _, reason := range []string{"The run was against the wrong commit.", "Another reason under the same key."} {
				before := rig.written(t, own, parent)
				private, missing := keyed(result, key, reason), keyed(missingResult, key, reason)
				if after := rig.written(t, own, parent); after != before {
					t.Errorf("the keyed retractions under %q changed %s to %s, want nothing written", key, before, after)
				}
				want := strings.ReplaceAll(missing.Body.String(), missingResult, result)
				if private.Code != http.StatusForbidden || private.Code != missing.Code || private.Body.String() != want ||
					private.Header().Get("Idempotency-Key") != missing.Header().Get("Idempotency-Key") {
					t.Errorf("under the key %q the private check's retraction answers %d %s (key %q), and a missing result's %d %s (key %q), want one 403",
						key, private.Code, private.Body.String(), private.Header().Get("Idempotency-Key"),
						missing.Code, missing.Body.String(), missing.Header().Get("Idempotency-Key"))
				}
			}
		}
		requireTaken(t, "the retraction of the token that names the private project", retract(namingToken, result, "The run was against the wrong commit.")(), http.StatusCreated)
	})
}

// TestAStatusChangeLeavesOutAPrivateProjectsIssue: planStatusChangeUnderLocks leaves out of a
// status change of an issue of the token's own project each issue of a private project that the
// token does not name. The call does not move or unlink that issue, and its answer and its dry run
// name it as no step and hold its title nowhere. The rest of the cascade runs. Each case starts
// from issues of its own:
//
//   - include_parent over a private parent: the issue moves into work and its parent stays where
//     it is. The rule judges that pair, so the dry run, which answers as the rule does whatever
//     the setting, answers 409 PARENT_STATUS_CONFLICT, and its issues leave the parent out. The
//     issue's own parent field still holds the parent's key, as it did before the call. The parent
//     hears the issue's child.status event, as the parent of every child whose status moves does.
//   - include_children into work over a private child: the parent moves and the child stays in
//     triage.
//   - unlink_children over a private child: the parent moves and the child stays under it.
//
// The token that names the private project moves or unlinks that issue in the same call.
func TestAStatusChangeLeavesOutAPrivateProjectsIssue(t *testing.T) {
	t.Parallel()
	rig := newPrivateProjectRig(t, testServerOptions{})
	type family struct{ own, private, privateTitle string }
	// above files a private parent and an issue of the token's own project under it.
	above := func(title string) family {
		privateTitle := title + ", the private parent"
		private := rig.issue(t, privateTestProject, privateTitle, nil)
		return family{own: rig.issue(t, brokerTestProject, title+", the issue", map[string]any{"parent": private}), private: private, privateTitle: privateTitle}
	}
	// below files a parent of the token's own project and a private child under it.
	below := func(title string) family {
		own := rig.issue(t, brokerTestProject, title+", the parent", nil)
		privateTitle := title + ", the private child"
		return family{own: own, private: rig.issue(t, privateTestProject, privateTitle, map[string]any{"parent": own}), privateTitle: privateTitle}
	}
	cases := []struct {
		name string
		body map[string]any
		// scoped and named are the token's family and the naming token's.
		scoped, named family
		// dryRun is the status of the scoped token's dry run.
		dryRun int
		// own and private are the status and the parent ("" for none) of the own issue and of the
		// private issue after the scoped token's call; moved is the private issue's after the
		// naming token's.
		own, private, moved [2]string
	}{
		{
			name: "include_parent over a private parent", body: map[string]any{"status": "in_progress", "include_parent": true},
			scoped: above("Started under a private parent"), named: above("Started by the naming token"),
			dryRun: http.StatusConflict,
		},
		{
			name: "include_children into work over a private child", body: map[string]any{"status": "todo", "include_children": true},
			scoped: below("Picked up over a private child"), named: below("Picked up by the naming token"),
			dryRun: http.StatusOK,
		},
		{
			name: "unlink_children over a private child", body: map[string]any{"status": "backlog", "unlink_children": true},
			scoped: below("Put back over a private child"), named: below("Put back by the naming token"),
			dryRun: http.StatusOK,
		},
	}
	rig.makePrivate(t)
	cases[0].own, cases[0].private, cases[0].moved = [2]string{"in_progress", cases[0].scoped.private}, [2]string{model.IssueStatusTriage, ""}, [2]string{"in_progress", ""}
	cases[1].own, cases[1].private, cases[1].moved = [2]string{"todo", ""}, [2]string{model.IssueStatusTriage, cases[1].scoped.own}, [2]string{"todo", cases[1].named.own}
	cases[2].own, cases[2].private, cases[2].moved = [2]string{"backlog", ""}, [2]string{model.IssueStatusTriage, cases[2].scoped.own}, [2]string{model.IssueStatusTriage, ""}
	checker := ruleRig{database: rig.database}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			scoped := test.scoped
			path := "/api/v1/issues/" + scoped.own + "/status-change"
			// leftOut fails t unless response names the private issue as no entry of changed and
			// of issues, holds its title nowhere, and names its key nowhere but in the own issue's
			// parent field.
			leftOut := func(t *testing.T, what string, response *httptest.ResponseRecorder) {
				t.Helper()
				answer := decodeBody[struct {
					Issue struct {
						Parent *string `json:"parent"`
					} `json:"issue"`
					Changed []struct {
						Key    string  `json:"key"`
						Parent *string `json:"parent"`
					} `json:"changed"`
					Issues []struct {
						Key string `json:"key"`
					} `json:"issues"`
					Error string `json:"error"`
				}](t, response)
				body := response.Body.String()
				for _, entry := range answer.Changed {
					if entry.Key == scoped.private {
						t.Errorf("%s names %s in changed: %s", what, scoped.private, body)
					}
				}
				for _, issue := range answer.Issues {
					if issue.Key == scoped.private {
						t.Errorf("%s names %s in issues: %s", what, scoped.private, body)
					}
				}
				if strings.Contains(body, scoped.privateTitle) || strings.Contains(answer.Error, scoped.private) {
					t.Errorf("%s names %s or its title: %s", what, scoped.private, body)
				}
				if test.own[1] != scoped.private && strings.Contains(body, scoped.private) {
					t.Errorf("%s names %s: %s", what, scoped.private, body)
				}
			}
			before := rig.written(t, scoped.own, scoped.private)
			planned := rig.send(t, scopedToken, http.MethodPost, path, withMembers(test.body, map[string]any{"dry_run": true}))
			if planned.Code != test.dryRun {
				t.Fatalf("the dry run: status=%d body=%s, want %d", planned.Code, planned.Body.String(), test.dryRun)
			}
			leftOut(t, "the dry run", planned)
			if after := rig.written(t, scoped.own, scoped.private); after != before {
				t.Fatalf("the dry run changed %s to %s, want nothing written", before, after)
			}

			privateBefore := rig.written(t, scoped.private)
			moved := rig.send(t, scopedToken, http.MethodPost, path, test.body)
			requireTaken(t, "the status change", moved, http.StatusOK)
			leftOut(t, "the status change", moved)
			checker.requireIssue(t, scoped.own, test.own[0], test.own[1])
			checker.requireIssue(t, scoped.private, test.private[0], test.private[1])
			// The private issue's row, events, comments and asks stay as they were: the call wrote
			// no step of it. The issues count and the own issue's events moved with the call's own
			// writes. A private parent hears the child.status event of its own project's child,
			// which every status write of a child appends on its parent, so its status and parent
			// are what stays.
			if test.own[1] != scoped.private {
				if after := rig.written(t, scoped.private); privateRow(after) != privateRow(privateBefore) {
					t.Errorf("the status change changed %s to %s, want the private issue as it was", privateBefore, after)
				}
			}

			named := test.named
			requireTaken(t, "the status change of the token that names the private project",
				rig.send(t, namingToken, http.MethodPost, "/api/v1/issues/"+named.own+"/status-change", test.body), http.StatusOK)
			checker.requireIssue(t, named.private, test.moved[0], test.moved[1])
		})
	}
}

// privateRow is the part of written's answer that holds the issues' rows and their events,
// comments and asks, which a status change of other issues leaves as they were.
func privateRow(written string) string {
	parts := strings.Split(written, "; ")
	kept := []string{parts[0]}
	for _, part := range parts[1:] {
		if strings.HasPrefix(part, "events ") || strings.HasPrefix(part, "comments ") || strings.HasPrefix(part, "asks ") {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "; ")
}

// TestAParentStatusConflictLeavesOutAPrivateProjectsIssue: with the parent status rule on, a
// parent of the token's own project has an open child in the private project. The rule judges
// every pair, the private child included, so each call below that would leave the parent out of
// line is refused and changes nothing, and its answer names the child nowhere:
//
//   - a status change to done with include_children: 403 BROWSER_SIGN_IN_REQUIRED, which a request
//     that presents a token gets before the rule, also for its dry run.
//   - a status change to done, and its dry run: 409 PARENT_STATUS_CONFLICT with no issue listed.
//   - a status change to backlog with include_children over a child in work, and its dry run: 409
//     PARENT_STATUS_CONFLICT with no issue listed, since the call leaves the child out.
//   - a PATCH of the status to done: 409 PARENT_STATUS_CONFLICT with no issue listed.
//
// With the token that names the private project, the status change to done and the PATCH list the
// child, and the status change to backlog with include_children moves the child and is taken. The
// rule refuses an acceptance write over such a child before its open-child check, so
// TestAnAcceptanceWriteLeavesOutAPrivateProjectsOpenChild holds its 409 FOREIGN_CHILD_OPEN with the
// rule off.
func TestAParentStatusConflictLeavesOutAPrivateProjectsIssue(t *testing.T) {
	t.Parallel()
	rig := newPrivateProjectRig(t, testServerOptions{parentStatusRule: true})
	type family struct{ parent, child, title string }
	// family files a parent of the token's own project and an open private child under it. With
	// started, a person moves the child, and so the parent, into work.
	newFamily := func(title string, started bool) family {
		parent := rig.issue(t, brokerTestProject, title+", the parent", nil)
		childTitle := title + ", the private child"
		child := rig.issue(t, privateTestProject, childTitle, map[string]any{"parent": parent})
		if started {
			requireTaken(t, "move the private child into work", rig.asPerson(t, http.MethodPost, "/api/v1/issues/"+child+"/status-change", map[string]any{
				"status": "in_progress", "include_parent": true,
			}), http.StatusOK)
		}
		return family{parent: parent, child: child, title: childTitle}
	}
	closeWithChildren := newFamily("Closed with its children", false)
	closed := newFamily("Closed alone", false)
	putBack := newFamily("Put back with its children", true)
	patched := newFamily("Closed by a PATCH", false)
	rig.makePrivate(t)

	// conflict is the whole of a refusal as these cases read it.
	type conflict struct {
		Code   string `json:"code"`
		Error  string `json:"error"`
		Issues []struct {
			Key string `json:"key"`
		} `json:"issues"`
	}
	// refused fails t unless send answers status and code, changes neither issue of f, and lists
	// want as its issues; its answer names the child nowhere unless want lists it.
	refused := func(t *testing.T, what string, f family, status int, code string, want []string, send func() *httptest.ResponseRecorder) {
		t.Helper()
		before := rig.written(t, f.parent, f.child)
		response := send()
		if after := rig.written(t, f.parent, f.child); after != before {
			t.Errorf("%s changed %s to %s, want nothing written", what, before, after)
		}
		answer := decodeBody[conflict](t, response)
		if response.Code != status || answer.Code != code {
			t.Fatalf("%s: status=%d body=%s, want %d %s", what, response.Code, response.Body.String(), status, code)
		}
		listed := []string{}
		for _, issue := range answer.Issues {
			listed = append(listed, issue.Key)
		}
		if code == "PARENT_STATUS_CONFLICT" && !slices.Equal(listed, want) {
			t.Errorf("%s lists %v, want %v: %s", what, listed, want, response.Body.String())
		}
		if len(want) == 0 && (strings.Contains(response.Body.String(), f.child) || strings.Contains(response.Body.String(), f.title)) {
			t.Errorf("%s names the private child %s: %s", what, f.child, response.Body.String())
		}
	}
	statusChange := func(token string, f family, body map[string]any) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder {
			return rig.send(t, token, http.MethodPost, "/api/v1/issues/"+f.parent+"/status-change", body)
		}
	}
	dryRun := func(body map[string]any) map[string]any { return withMembers(body, map[string]any{"dry_run": true}) }

	t.Run("a status change to done with include_children", func(t *testing.T) {
		body := map[string]any{"status": "done", "include_children": true}
		refused(t, "the call", closeWithChildren, http.StatusForbidden, "BROWSER_SIGN_IN_REQUIRED", nil, statusChange(scopedToken, closeWithChildren, body))
		refused(t, "the dry run", closeWithChildren, http.StatusForbidden, "BROWSER_SIGN_IN_REQUIRED", nil, statusChange(scopedToken, closeWithChildren, dryRun(body)))
	})

	t.Run("a status change to done", func(t *testing.T) {
		body := map[string]any{"status": "done"}
		refused(t, "the call", closed, http.StatusConflict, "PARENT_STATUS_CONFLICT", []string{}, statusChange(scopedToken, closed, body))
		refused(t, "the dry run", closed, http.StatusConflict, "PARENT_STATUS_CONFLICT", []string{}, statusChange(scopedToken, closed, dryRun(body)))
		refused(t, "the call of the token that names the private project", closed, http.StatusConflict, "PARENT_STATUS_CONFLICT",
			[]string{closed.child}, statusChange(namingToken, closed, body))
		refused(t, "the dry run of the token that names the private project", closed, http.StatusConflict, "PARENT_STATUS_CONFLICT",
			[]string{closed.child}, statusChange(namingToken, closed, dryRun(body)))
	})

	t.Run("a status change to backlog with include_children over a child in work", func(t *testing.T) {
		body := map[string]any{"status": "backlog", "include_children": true}
		refused(t, "the call", putBack, http.StatusConflict, "PARENT_STATUS_CONFLICT", []string{}, statusChange(scopedToken, putBack, body))
		refused(t, "the dry run", putBack, http.StatusConflict, "PARENT_STATUS_CONFLICT", []string{}, statusChange(scopedToken, putBack, dryRun(body)))
		requireTaken(t, "the call of the token that names the private project", statusChange(namingToken, putBack, body)(), http.StatusOK)
		ruleRig{database: rig.database}.requireIssue(t, putBack.child, "backlog", putBack.parent)
	})

	t.Run("a PATCH of the status to done", func(t *testing.T) {
		patch := func(token string) func() *httptest.ResponseRecorder {
			return func() *httptest.ResponseRecorder {
				return rig.send(t, token, http.MethodPatch, "/api/v1/issues/"+patched.parent, map[string]any{"status": "done"})
			}
		}
		refused(t, "the PATCH", patched, http.StatusConflict, "PARENT_STATUS_CONFLICT", []string{}, patch(scopedToken))
		refused(t, "the PATCH of the token that names the private project", patched, http.StatusConflict, "PARENT_STATUS_CONFLICT",
			[]string{patched.child}, patch(namingToken))
	})
}

// TestAnAcceptanceWriteLeavesOutAPrivateProjectsOpenChild: an acceptance write, a PATCH of an
// issue's status to done that carries refuse_open_foreign_children, on an issue of the token's own
// project whose open child is in the private project, answers 409 FOREIGN_CHILD_OPEN with the
// child absent from issues and from the message, and changes nothing: the child is open, so the
// write is refused with no child listed. The token that names the private project, and a person,
// get the same 409 with the child listed. The parent status rule is off, since with it on R24
// refuses a close over an open child first
// (TestTheParentStatusRuleDecidesBeforeTheOpenForeignChildCheck).
func TestAnAcceptanceWriteLeavesOutAPrivateProjectsOpenChild(t *testing.T) {
	t.Parallel()
	rig := newPrivateProjectRig(t, testServerOptions{settle: time.Hour})
	parent := rig.issue(t, brokerTestProject, "Accepted over a private child", nil)
	childTitle := "The open private child of an accepted issue"
	child := rig.issue(t, privateTestProject, childTitle, map[string]any{"parent": parent})
	rig.makePrivate(t)
	body := map[string]any{"status": "done", "refuse_open_foreign_children": true}
	listed := []string{child + " " + privateTestProject + " " + model.IssueStatusTriage}
	for _, write := range []struct {
		name string
		send func(t *testing.T) *httptest.ResponseRecorder
		want []string
	}{
		{"the token that names another project", func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, scopedToken, http.MethodPatch, "/api/v1/issues/"+parent, body)
		}, []string{}},
		{"the token that names the private project", func(t *testing.T) *httptest.ResponseRecorder {
			return rig.send(t, namingToken, http.MethodPatch, "/api/v1/issues/"+parent, body)
		}, listed},
		{"a person", func(t *testing.T) *httptest.ResponseRecorder {
			return rig.asPerson(t, http.MethodPatch, "/api/v1/issues/"+parent, body)
		}, listed},
	} {
		t.Run(write.name, func(t *testing.T) {
			before := rig.written(t, parent, child)
			response := write.send(t)
			if after := rig.written(t, parent, child); after != before {
				t.Errorf("the refused write changed %s to %s, want nothing written", before, after)
			}
			if response.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s, want 409 FOREIGN_CHILD_OPEN", response.Code, response.Body.String())
			}
			refusal := decodeBody[foreignChildOpenAnswer](t, response)
			got := []string{}
			for _, issue := range refusal.Issues {
				got = append(got, issue.Key+" "+issue.Project+" "+issue.Status)
			}
			if refusal.Code != "FOREIGN_CHILD_OPEN" || !slices.Equal(got, write.want) {
				t.Errorf("the refusal lists %v, want FOREIGN_CHILD_OPEN listing %v: %s", got, write.want, response.Body.String())
			}
			names := strings.Contains(response.Body.String(), child) || strings.Contains(response.Body.String(), childTitle)
			if len(write.want) == 0 && names {
				t.Errorf("the refusal names the private child %s: %s", child, response.Body.String())
			}
			if len(write.want) > 0 && !strings.Contains(refusal.Error, child) {
				t.Errorf("the refusal's message %q does not name the child %s", refusal.Error, child)
			}
		})
	}
}
