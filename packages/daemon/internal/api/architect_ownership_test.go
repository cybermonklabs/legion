package api

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A sub-architect holds a grant for the whole tree, so it can name any issue of it, but
// release_children, park_child and rerun_child move an issue into or out of the workflow, which is
// the work of an architect above that issue. So the architect of a child C takes them only on an
// issue under C: the tree root, C itself, its sibling S and every issue under S are refused
// ISSUE_NOT_OWNED before anything about the issue's status or run is answered, and the refusal
// applies nothing and writes nothing to Dispatch. A wave that names one such issue releases none of
// the wave. An issue under C gets each route's own answers, and an issue of another tree is
// outside the tree, as before. The root architect still takes all three on every issue of its
// tree, S's included.
func TestASubArchitectReleasesParksAndRerunsOnlyIssuesUnderItsOwn(t *testing.T) {
	const (
		root, own, sibling                         = "LEGION-208", "LEGION-209", "LEGION-210"
		ownTriage, ownRunning, ownDone             = "LEGION-211", "LEGION-212", "LEGION-213"
		siblingTriage, siblingRunning, siblingDone = "LEGION-214", "LEGION-215", "LEGION-216"
		foreign                                    = "LEGION-300"
	)
	h, facts, statuses := newArchitectHarness(t, nil, nil)
	seedTree(t, h, root, own, sibling)
	seedTree(t, h, foreign)
	// The record holds a child only once it is todo, so the triage grandchildren are Dispatch's
	// alone, and the running and done ones are recorded under their parents.
	records := record.NewStore()
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		for _, grandchild := range []struct {
			key, parent, status string
			at                  phase.Phase
		}{
			{ownRunning, own, "in_progress", phase.Implementing}, {ownDone, own, "done", phase.Done},
			{siblingRunning, sibling, "in_progress", phase.Implementing}, {siblingDone, sibling, "done", phase.Done},
		} {
			parent := grandchild.parent
			if err := records.PutIssue(context.Background(), tx, record.Issue{
				Key: grandchild.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: grandchild.key, Parent: &parent,
				Phase: grandchild.at, Status: grandchild.status,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("record the grandchildren: %v", err)
	}
	view := func(key, status, parent string) dispatch.Issue {
		issue := dispatch.Issue{Key: key, ResolvedRepo: "acme/widgets", Status: status}
		if parent != "" {
			issue.Parent = &parent
		}
		return issue
	}
	statuses.issues = map[string]dispatch.Issue{}
	for _, issue := range []dispatch.Issue{
		view(root, "in_progress", ""), view(own, "in_progress", root), view(sibling, "in_progress", root),
		view(ownTriage, "triage", own), view(ownRunning, "in_progress", own), view(ownDone, "done", own),
		view(siblingTriage, "triage", sibling), view(siblingRunning, "in_progress", sibling), view(siblingDone, "done", sibling),
		view(foreign, "backlog", ""),
	} {
		statuses.issues[issue.Key] = issue
	}
	rootArchitect := newLiveClaim(t, h, root, claim.RoleArchitect)
	subArchitect := newLiveClaimIn(t, h, root, own, claim.RoleArchitect)

	type route struct {
		op, path, writes string
		body             func(grant, issue string) map[string]any
	}
	child := func(grant, issue string) map[string]any { return map[string]any{"grantId": grant, "issue": issue} }
	release := route{"release_children", "/legion/v1/waves/release", "todo", func(grant, issue string) map[string]any {
		return map[string]any{"grantId": grant, "issues": []string{issue}}
	}}
	park := route{"park_child", "/legion/v1/children/park", "backlog", child}
	rerun := route{"rerun_child", "/legion/v1/children/rerun", "todo", child}

	type answer struct {
		status int
		code   string
	}
	var (
		applied     = answer{http.StatusOK, ""}
		notOwned    = answer{http.StatusForbidden, "ISSUE_NOT_OWNED"}
		outside     = answer{http.StatusForbidden, "ISSUE_OUTSIDE_TREE"}
		notRecorded = answer{http.StatusNotFound, "ISSUE_NOT_FOUND"}
		released    = answer{http.StatusConflict, "ISSUE_ALREADY_RELEASED"}
		running     = answer{http.StatusConflict, "CHILD_RUNNING"}
		notRunning  = answer{http.StatusConflict, "CHILD_NOT_RUNNING"}
	)
	// call makes architect's request of the route on issue and checks its answer: an applied call
	// writes the route's one status for issue, and a refused one applies no fact and writes nothing.
	call := func(architect liveClaim, on route, issue string, want answer) {
		t.Helper()
		factsBefore, writesBefore := len(facts.recorded()), len(statuses.writes)
		recorder := h.request(http.MethodPost, on.path, on.body(architect.grant(t), issue), nil)
		if want == applied {
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s of %s by the architect of %s = %d %s, want 200", on.op, issue, architect.issue, recorder.Code, recorder.Body)
			}
			if got := statuses.writes[writesBefore:]; !reflect.DeepEqual(got, []statusWrite{{issue: issue, status: on.writes}}) {
				t.Fatalf("%s of %s by the architect of %s wrote %#v to Dispatch, want %s only", on.op, issue, architect.issue, got, on.writes)
			}
			return
		}
		body := recorder.Body.String()
		var failure Failure
		decodeInto(t, recorder, &failure)
		if recorder.Code != want.status || failure.Code != want.code {
			t.Fatalf("%s of %s by the architect of %s = %d %s, want %d %s", on.op, issue, architect.issue, recorder.Code, body, want.status, want.code)
		}
		if want == notOwned && !strings.Contains(body, on.op+" of "+issue) {
			t.Fatalf("%s of %s by the architect of %s: refusal %s does not name the operation", on.op, issue, architect.issue, body)
		}
		if got := facts.recorded()[factsBefore:]; len(got) != 0 {
			t.Fatalf("%s of %s by the architect of %s was refused %s but applied %#v", on.op, issue, architect.issue, want.code, got)
		}
		if got := statuses.writes[writesBefore:]; len(got) != 0 {
			t.Fatalf("%s of %s by the architect of %s was refused %s but wrote %#v to Dispatch", on.op, issue, architect.issue, want.code, got)
		}
	}

	// The wave is judged whole before anything is written: the sibling refuses the child under C
	// named beside it too.
	wave := h.request(http.MethodPost, release.path, map[string]any{"grantId": subArchitect.grant(t), "issues": []string{ownTriage, sibling}}, nil)
	refused(t, wave, http.StatusForbidden, "ISSUE_NOT_OWNED")
	if len(statuses.writes) != 0 || len(facts.recorded()) != 0 {
		t.Fatalf("a refused wave wrote %#v to Dispatch and applied %#v", statuses.writes, facts.recorded())
	}

	// Each row is one issue the architect of C names, and what each route answers it, in this
	// order. A triage issue is not recorded, so park_child and rerun_child do not find it; the
	// calls under C that apply leave the record as it was, since the harness's handler only
	// records the fact.
	for _, row := range []struct {
		issue                string
		release, park, rerun answer
	}{
		{root, notOwned, notOwned, notOwned},
		{own, notOwned, notOwned, notOwned},
		{sibling, notOwned, notOwned, notOwned},
		{siblingTriage, notOwned, notRecorded, notRecorded},
		{siblingRunning, notOwned, notOwned, notOwned},
		{siblingDone, notOwned, notOwned, notOwned},
		{foreign, outside, outside, outside},
		{ownTriage, applied, notRecorded, notRecorded},
		{ownRunning, released, applied, running},
		{ownDone, released, notRunning, applied},
	} {
		call(subArchitect, release, row.issue, row.release)
		call(subArchitect, park, row.issue, row.park)
		call(subArchitect, rerun, row.issue, row.rerun)
	}
	// The root architect owns every issue of its tree, the sibling's grandchildren included.
	call(rootArchitect, release, siblingTriage, applied)
	call(rootArchitect, park, siblingRunning, applied)
	call(rootArchitect, rerun, siblingDone, applied)

	want := []statusWrite{
		{issue: ownTriage, status: "todo"}, {issue: ownRunning, status: "backlog"}, {issue: ownDone, status: "todo"},
		{issue: siblingTriage, status: "todo"}, {issue: siblingRunning, status: "backlog"}, {issue: siblingDone, status: "todo"},
	}
	if !reflect.DeepEqual(statuses.writes, want) {
		t.Fatalf("Dispatch writes = %#v, want only the applied calls' %#v", statuses.writes, want)
	}
	var appliedTo []string
	for _, fact := range facts.recorded() {
		issue, ok := fact.(intake.DispatchIssue)
		if !ok {
			t.Fatalf("fact %#v is not an applied status write", fact)
		}
		appliedTo = append(appliedTo, issue.Key)
	}
	if wantKeys := []string{ownTriage, ownRunning, ownDone, siblingTriage, siblingRunning, siblingDone}; !reflect.DeepEqual(appliedTo, wantKeys) {
		t.Fatalf("facts applied to %q, want one per applied call: %q", appliedTo, wantKeys)
	}
}
