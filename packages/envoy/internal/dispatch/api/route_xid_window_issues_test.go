//go:build !race

package api

import (
	"cmp"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// issueRouteXidWindows is the rows of the route window table (route_xid_window_test.go) for the
// routes that write an issue or a project: the reopen, the design gates, the role holders, the
// project's settings, the handoff, the claim's release, the creates and patches that name a
// repository, an advances relation or a blocker in another project, and the create of a near copy
// the gate's refusal of which could not be recorded.
var issueRouteXidWindows = []routeXidWindowCase{
	// The reopen's document work - the withdrawn asks read back, their blocks and copies written
	// open into the spec (prepareRestoration, docs.WriteIssueAskStates), the mark of the documents
	// whose ask blocks wait for the reopen (docs.MarkReopenOwedAskDocuments), the restored asks'
	// parts (recordRestoredAsksTx, queueAskParts) and the follow of the versions a settlement wrote
	// while the issue was closed (docs.FollowClosedIssueVersions) - runs before the issue's update
	// and queues its writes, so none of its reads is inside the window, however many asks the
	// reopen restores; this fixture restores two.
	{
		name: "the issue reopen", route: "PATCH /api/v1/issues/{key} reopening a closed issue (patchIssue, prepareRestoration)",
		ready: readyIssueReopen, closedIssue: true, window: storetest.XidWindowOf(5,
			"the issue row's own update, the two reads that load the issue it answers with (its row "+
				"with its advances chain, then its external links), the batch restoring its asks, writing "+
				"their blocks, marking its owed documents, indexing the asks' parts and appending "+
				"ask.opened for each, and the COMMIT ending them"),
	},
	// The delete of the gates a publish no longer names writes nothing on this fixture's first
	// publish, so the window opens at the upsert; a publish that drops a gate opens it at the
	// delete, and the shape counts it.
	{
		name: "the design gates route", route: "PUT /api/v1/issues/{key}/design-gates (putIssueDesignGates)",
		ready: readyDesignGates, window: storetest.XidWindowOf(3,
			"the delete of the gates the publish no longer names, the upsert of the ones it names and the COMMIT ending them"),
	},
	// The issue holds the tester ses-a and the publication moves it to ses-b (readyRoleHolders),
	// so the window opens at the delete of the pair the publication no longer names. The read of
	// the stored set the route answers with runs after the insert, in the same transaction, so it
	// is inside the window too.
	{
		name: "the role holders route", route: "PUT /api/v1/issues/{key}/role-holders (putIssueRoleHolders)",
		ready: readyRoleHolders, window: storetest.XidWindowOf(4,
			"the delete of the pairs the publication no longer names, the insert of the ones not stored yet, "+
				"the read of the stored set it answers with and the COMMIT ending them"),
	},
	{
		name: "the project default assignee route", route: "PUT /api/v1/projects/{key}/default-assignee (putProjectDefaultAssignee)",
		ready: readyProjectDefaultAssignee, window: storetest.XidWindowOf(3,
			"the project row's update, the batch appending settings.project_default_assignee.updated and the COMMIT ending them"),
	},
	{
		name: "the project gated steps route", route: "PUT /api/v1/projects/{key}/gated-steps (putProjectGatedSteps)",
		ready: readyProjectGatedSteps, window: storetest.XidWindowOf(3,
			"the project row's update, the batch appending project.updated and the COMMIT ending them"),
	},
	{
		name: "the handoff route", route: "POST /api/v1/issues/{key}/handoffs/{phase} (recordHandoff)",
		ready: readyHandoff, window: storetest.XidWindowOf(3,
			"the handoff's insert, the batch appending handoff.recorded and the COMMIT ending them"),
	},
	{
		name: "the claim release route", route: "DELETE /api/v1/issues/{key}/claim (releaseIssueClaim, claimWrite)",
		ready: readyClaimRelease, window: storetest.XidBatchedWindow,
	},
	// A patch naming a repository writes it in the issue row's own update and reads or locks no
	// repo_projects row: an issue's own repository needs no mapping (INFRA-2543 R3,
	// parseIssueRepo), so this one names a repository no project maps. It closes the issue
	// too, so its read phase holds the close's document work - the block ask withdrawn and its
	// block written resolved into the spec (prepareWithdrawal) - and a session's write advice,
	// none of it inside the window.
	{
		name: "the issue patch setting its repository while closing it", route: "PATCH /api/v1/issues/{key} naming a repository (patchIssue, prepareWithdrawal)",
		ready: readyIssuePatchNamingRepository, window: storetest.XidWindowOf(5,
			"the issue row's own update, the two reads that load the issue it answers with, "+
				"the batch withdrawing its asks and its COMMIT"),
	},
	// A create naming a repository writes it in the issue row's insert and reads or locks no
	// repo_projects row (INFRA-2543 R3). Its read phase, under the project's owner lock, holds its
	// spec's seed, task count and reference index, all of the harness's size, and its write
	// advice.
	{
		name: "the issue create naming a repository", route: "POST /api/v1/issues naming a repository (createIssue)",
		ready: readyIssueCreateNamingRepository, window: storetest.XidWindowOf(10,
			"the create's inline steering writes - the project's next number, the rank lock, the rank read, the issue row, its blocked_by links and its artifact row - "+
				"the two reads that load the issue it answers with, the queued batch and its COMMIT"),
	},
	// A near copy whose refusal's near_copy_checks row could not be written goes in unchecked
	// (nearCopyOutcome.unrecorded) and writes its own row in its transaction (recordInTx,
	// writeNearCopyCheck): one inline write more, after its first write and so after the owner
	// lock it takes in its read phase, where its spec's seed, task count and reference index, all
	// of the harness's size, and its write advice run. The refusal's own write, which errs, is a
	// statement of its own before the transaction opens (recordNearCopyRefusal), and holds no
	// transaction id of the create's.
	{
		name: "the issue create of a near copy whose refusal was not recorded", route: "POST /api/v1/issues past an unrecorded refusal (createIssue, writeNearCopyCheck)",
		ready: readyIssueCreateUnrecordedNearCopy, window: storetest.XidWindowOf(11,
			"the create's inline steering writes - the project's next number, the rank lock, the rank read, the issue row, its blocked_by links and its artifact row - "+
				"the two reads that load the issue it answers with, its near_copy_checks row, the queued batch and its COMMIT"),
	},
	// A patch stating a relation to a priority decides the revision rule on the issue it read
	// under its owner lock (refuseAdvancesRevision) and writes the relation after the row's own
	// update (writeIssueAdvances), whose reads of the issue and of the priority's state are its
	// write phase's.
	{
		name: "the issue patch stating an advances relation", route: "PATCH /api/v1/issues/{key} naming advances (patchIssue, writeIssueAdvances)",
		ready: readyIssuePatchStatingAdvances, window: storetest.XidWindowOf(8,
			"the issue row's own update, the reads of the issue and of the priority it advances, the relation's tuple, "+
				"the two reads that load the issue it answers with, the batch appending issue.updated and its COMMIT"),
	},
	// A create naming a blocker in another project (INFRA-3036) takes the dependency locks of both
	// projects and the deployment's own before its project's owner lock, by plain reads and
	// advisory locks (lockIssueDependencyWrite), so its read phase - the spec's seed, task count,
	// reference index and the write advice - still holds no transaction id; writeBlockedBy reads
	// the target's project and walks the wait graph inside the write phase, under those locks,
	// and the event it appends names the blocker among its changed references, which one more read
	// resolves (refs.resolveIssueTargets). A window with one blocker and a wait graph one link deep.
	{
		name: "the issue create naming a blocker in another project", route: "POST /api/v1/issues naming a blocked_by target in another project (createIssue, lockIssueDependencyWrite, writeBlockedBy)",
		ready: readyIssueCreateBlockedAcrossProjects, window: storetest.XidWindowOf(14,
			"the create's inline steering writes - the project's next number, the rank lock, the rank read, the issue row, "+
				"the target's project read, the wait walk, the blocked_by delete and insert and its artifact row - "+
				"the two reads that load the issue it answers with, the read resolving the blocker its event names, "+
				"the queued batch and its COMMIT"),
	},
	// A patch naming a blocker in another project takes the same locks before the issue's owner
	// lock, and its write phase is the row's update and writeBlockedBy's reads and writes.
	{
		name: "the issue patch naming a blocker in another project", route: "PATCH /api/v1/issues/{key} naming a blocked_by target in another project (patchIssue, lockIssueDependencyWrite, writeBlockedBy)",
		ready: readyIssuePatchBlockedAcrossProjects, window: storetest.XidWindowOf(10,
			"the issue row's own update, the target's project read, the wait walk, the blocked_by delete and insert, "+
				"the two reads that load the issue it answers with, the read resolving the blocker its event names, "+
				"the batch appending issue.updated and its COMMIT"),
	},
}

// readyIssueReopen drives PATCH /api/v1/issues/{key} reopening a closed issue whose close
// withdrew an anchored ask and the ask settlement opened from the spec's `:::ask` block, so the
// reopen restores both and writes the block's state back into the spec (prepareRestoration).
func readyIssueReopen(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	base.spec += "\n" + transportAsk
	fixture := base.issue(t, "Reopen")
	awaitIndexedAskBlock(t, fixture.handler, fixture.artifactID, "decision", "Which transport?")
	_, quotes := xidSpec(1)
	if created := sessionRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/asks", map[string]any{
		"question": "Who owns this requirement?",
		"options":  []map[string]string{{"label": "Platform"}, {"label": "Product"}},
		"anchor":   map[string]any{"artifact": "spec", "quote": quotes[0]}, "actor": sessionActor(),
	}); created.Code != http.StatusCreated {
		t.Fatalf("open the anchored ask: status=%d body=%s", created.Code, created.Body.String())
	}
	if closed := dispatchRequest(t, fixture.handler, http.MethodPatch, "/api/v1/issues/"+fixture.issueKey,
		map[string]any{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close the issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	return fixture.routeWrite(t, "the reopened issue",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+fixture.issueKey,
				map[string]any{"status": "todo"}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("reopen the issue: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if restored := fixture.rowCount(t, `select count(*) from asks where issue_key = $1 and state = 'open'`, fixture.issueKey); restored != 2 {
				t.Fatalf("the reopened issue holds %d open asks, want the two its close withdrew", restored)
			}
		})
}

// readyIssuePatchNamingRepository drives PATCH /api/v1/issues/{key} by a session that sets the
// issue's repository to one no project maps and closes it in the same request: the close
// withdraws the ask settlement opened from the spec's `:::ask` block and writes the block resolved
// into the spec (prepareWithdrawal), and the session's write advice is read, all before the
// patch's first write.
func readyIssuePatchNamingRepository(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	base.spec += "\n" + transportAsk
	fixture := base.issue(t, "Patch naming a repository")
	awaitIndexedAskBlock(t, fixture.handler, fixture.artifactID, "decision", "Which transport?")
	return fixture.routeWrite(t, "the issue's repository and its close",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+fixture.issueKey, map[string]any{
				"status": "done", "repo": "owner/repo", "actor": sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("patch the issue: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if patched := decodeBody[model.Issue](t, answer); patched.Repo == nil || *patched.Repo != "owner/repo" || patched.Status != "done" {
				t.Fatalf("the patched issue = repo %v status %q, want owner/repo and done", patched.Repo, patched.Status)
			}
			if open := fixture.rowCount(t, `select count(*) from asks where issue_key = $1 and state = 'open'`, fixture.issueKey); open != 0 {
				t.Fatalf("the closed issue still holds %d open asks, want the block ask its close withdrew", open)
			}
		})
}

// readyIssueCreateNamingRepository drives POST /api/v1/issues by a session naming a repository no
// project maps as the new issue's repository, with a spec of the harness's size, whose seed,
// task count and reference index are the create's document work, read before its first write
// with the session's write advice.
func readyIssueCreateNamingRepository(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Create naming a repository")
	spec, _ := xidSpec(2)
	return fixture.routeWrite(t, "the new issue naming its repository",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "LOCK", "title": "A second spec naming its repository", "spec": spec,
				"repo": "owner/repo", "force": true, "actor": sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("create the issue: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if created := decodeBody[model.Issue](t, answer); created.Repo == nil || *created.Repo != "owner/repo" {
				t.Fatalf("the created issue's repo = %v, want owner/repo", created.Repo)
			}
		})
}

// readyIssueCreateUnrecordedNearCopy drives POST /api/v1/issues by a session filing a near copy of
// the fixture's issue - its title and its spec, of the harness's size - while near_copy_checks
// refuses every refused row (a trigger, as putUnseenRefusal's), so the gate's refusal cannot be
// recorded and the create goes in unchecked, its own row written in its transaction
// (nearCopyOutcome.unrecorded, writeNearCopyCheck).
func readyIssueCreateUnrecordedNearCopy(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	const title = "Create of a near copy whose refusal was not recorded"
	fixture := base.issue(t, title)
	for _, statement := range []string{
		`create function near_copy_checks_lose_refusal() returns trigger language plpgsql as $$
		begin raise exception 'the refusal''s row was not written'; end $$`,
		`create trigger near_copy_checks_lose_refusal before insert on near_copy_checks
		  for each row when (new.outcome = 'refused') execute function near_copy_checks_lose_refusal()`,
	} {
		if _, err := fixture.database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("make the refusal's write err: %v", err)
		}
	}
	spec := cmp.Or(base.spec, "A spec.\n")
	return fixture.routeWrite(t, "the near copy let in unchecked and its near_copy_checks row",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "LOCK", "title": title, "spec": spec, "actor": sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("create the near copy: status=%d body=%s", answer.Code, answer.Body.String())
			}
			created := decodeBody[model.Issue](t, answer)
			if rows := fixture.rowCount(t, `
				select count(*) from near_copy_checks
				 where issue_key = $1 and outcome = 'not_made' and reason = 'unrecorded'`, created.Key); rows != 1 {
				t.Fatalf("near_copy_checks holds %d unrecorded rows naming %s, want the one its transaction wrote", rows, created.Key)
			}
		})
}

// readyIssuePatchStatingAdvances drives PATCH /api/v1/issues/{key} naming one priority in
// advances, with no revision, which a single act needs none of (refuseAdvancesRevision).
func readyIssuePatchStatingAdvances(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Patch stating advances")
	createTestPriority(t, fixture.handler, map[string]any{"id": "ship-the-daemon", "title": "Ship the daemon"}, "alice")
	return fixture.routeWrite(t, "the issue's advances relation",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+fixture.issueKey,
				map[string]any{"advances": []string{"ship-the-daemon"}}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("state the advances relation: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if patched := decodeBody[model.Issue](t, answer); len(patched.Advances) != 1 || patched.Advances[0].ID != "ship-the-daemon" {
				t.Fatalf("the patched issue advances %+v, want ship-the-daemon", patched.Advances)
			}
		})
}

// readyIssueCreateBlockedAcrossProjects drives POST /api/v1/issues by a session filing an issue of
// the fixture's project with a spec of the harness's size, blocked by an issue of another project
// (lockIssueDependencyWrite, writeBlockedBy).
func readyIssueCreateBlockedAcrossProjects(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Create blocked across projects")
	target := createListedIssues(t, fixture.handler, "AWAY", 1)[0]
	spec, _ := xidSpec(2)
	return fixture.routeWrite(t, "the new issue blocked by another project's issue",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "LOCK", "title": "A second spec waiting on another project", "spec": spec,
				"blocked_by": []string{target}, "force": true, "actor": sessionActor(),
			})
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("create the issue: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if created := decodeBody[model.Issue](t, answer); len(created.BlockedBy) != 1 || created.BlockedBy[0] != target {
				t.Fatalf("the created issue's blocked_by = %v, want [%s]", created.BlockedBy, target)
			}
		})
}

// readyIssuePatchBlockedAcrossProjects drives PATCH /api/v1/issues/{key} naming an issue of another
// project as the issue's one blocker (lockIssueDependencyWrite, writeBlockedBy).
func readyIssuePatchBlockedAcrossProjects(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Patch blocked across projects")
	target := createListedIssues(t, fixture.handler, "AWAY", 1)[0]
	return fixture.routeWrite(t, "the issue's blocker in another project",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+fixture.issueKey,
				map[string]any{"blocked_by": []string{target}}, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("patch the issue's blocker: status=%d body=%s", answer.Code, answer.Body.String())
			}
			if patched := decodeBody[model.Issue](t, answer); len(patched.BlockedBy) != 1 || patched.BlockedBy[0] != target {
				t.Fatalf("the patched issue's blocked_by = %v, want [%s]", patched.BlockedBy, target)
			}
		})
}

// readyHandoff drives POST /api/v1/issues/{key}/handoffs/{phase}, a session's handoff of one
// phase, which writes its next version under the issue's owner lock (recordHandoff).
func readyHandoff(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Handoff")
	return fixture.routeWrite(t, "the handoff",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return postHandoff(t, handler, fixture.issueKey, "implement", `{"summary":"The migration section is in."}`)
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusCreated {
				t.Fatalf("record the handoff: status=%d body=%s", answer.Code, answer.Body.String())
			}
		})
}

// readyClaimRelease drives DELETE /api/v1/issues/{key}/claim on an issue a person claimed, which
// clears the claim (claimWrite).
func readyClaimRelease(t *testing.T, base routeFixture) lockedItemWrite {
	t.Helper()
	fixture := base.issue(t, "Claim release")
	if claimed := dispatchRequest(t, fixture.handler, http.MethodPost, "/api/v1/issues/"+fixture.issueKey+"/claim", map[string]any{}, "alice"); claimed.Code != http.StatusOK {
		t.Fatalf("claim the issue: status=%d body=%s", claimed.Code, claimed.Body.String())
	}
	return fixture.routeWrite(t, "the released claim",
		func(handler http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+fixture.issueKey+"/claim", nil, "alice")
		},
		func(t *testing.T, answer *httptest.ResponseRecorder) {
			if answer.Code != http.StatusOK {
				t.Fatalf("release the claim: status=%d body=%s", answer.Code, answer.Body.String())
			}
		})
}
