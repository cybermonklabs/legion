// check_states_publish_test.go holds what the settler publishes (check_states.go): one state of
// each check at each open head, one check.state_changed event for each change of a state and none
// where nothing changed, the payload each event carries, the set form of a head with no check, and
// the state of each head computed for its own repository. What it publishes at a head an open pull
// request has left is check_states_former_heads_test.go's. The rig is check_states_test.go's.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The heads of the issues with no check of their own (TestAHeadWithNoCheckGetsTheSetForm), beside
// eachHeadH1 to eachHeadH4.
const (
	checkStatesH5 = "f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5"
	checkStatesH6 = "a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6"
)

// TestAStateEventIsPublishedForEachCheckAtEachOpenHead: ACME-1 has the open pull requests #70 at
// H1 and #71 at H3. When the two heads appear, the settler publishes one state of each check at
// each head: no, for no_result. Then the tester records a fail of k1 at H1, and one more event
// comes: k1 at H1, no, for fail.
func TestAStateEventIsPublishedForEachCheckAtEachOpenHead(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	if events := settleOwed(t, rig); len(events) != 0 {
		t.Fatalf("with no open head, the settler published %v, want nothing", eventLines(events))
	}
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
	writeHead(t, rig.database, ciJobRepository, 71, eachHeadH3, "open", rig.issue)
	wantEventSet(t, "the two heads appear", settleOwed(t, rig),
		stateWant{"k1", eachHeadH1, "no", "no_result"}, stateWant{"k2", eachHeadH1, "no", "no_result"},
		stateWant{"k1", eachHeadH3, "no", "no_result"}, stateWant{"k2", eachHeadH3, "no", "no_result"})

	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "fail", eachHeadH1)
	wantEvents(t, "the tester's fail of k1 at H1", settleOwed(t, rig), stateWant{"k1", eachHeadH1, "no", "fail"})
}

// TestAChangeOfStatePublishesOneCheckStateChangedEvent: the tester's pass of k1 at H1 moves its
// state from no to yes, and the event log of ACME-1 then holds exactly one new event, on ACME-1,
// of the type check.state_changed, by the system actor of the check states.
func TestAChangeOfStatePublishesOneCheckStateChangedEvent(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
	settleOwed(t, rig)
	after := lastEventID(t, rig.database)

	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	settleOwed(t, rig)
	var types []string
	rows, err := rig.database.Pool.Query(context.Background(),
		`select type from events where issue_key = $1 and id > $2 and type <> 'result.recorded' order by id`, rig.issue, after)
	if err != nil {
		t.Fatalf("read the new events of %s: %v", rig.issue, err)
	}
	for rows.Next() {
		var eventType string
		if err := rows.Scan(&eventType); err != nil {
			t.Fatalf("read an event type: %v", err)
		}
		types = append(types, eventType)
	}
	rows.Close()
	if !slices.Equal(types, []string{model.CheckStateChangedEventType}) {
		t.Fatalf("the pass of k1 at H1 appended %v on %s beside its result.recorded, want exactly one %s", types, rig.issue, model.CheckStateChangedEventType)
	}
	events := checkStateEventsAfter(t, rig.database, after)
	wantEvents(t, "the tester's pass of k1 at H1", events, stateWant{"k1", eachHeadH1, "yes", ""})
	if events[0].Issue != rig.issue || events[0].Actor != model.CheckStatesActor {
		t.Fatalf("the event is on %s by %+v, want on %s by %+v", events[0].Issue, events[0].Actor, rig.issue, model.CheckStatesActor)
	}
}

// TestTheStateEventNamesTheCheckTheHeadItsPullRequestTheStateAndTheReason: the tester records a
// fail of k1 at H1 before the first settle. The event of k1 names the check and its version, the
// repository and the head H1, the pull requests [70], the state no and the reason fail, the
// result that decides the state, the list [k1, k2], an empty ci_case and the serial 1, and
// @legion/contracts' CheckStateChangedEventPayloadSchema parses the payload as stored.
func TestTheStateEventNamesTheCheckTheHeadItsPullRequestTheStateAndTheReason(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	settleOwed(t, rig)
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
	fail := eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "fail", eachHeadH1)
	version := checkOf(t, readResults(t, rig.handler, rig.issue), "k1").Version

	events := eventsOfCheck(settleOwed(t, rig), "k1", eachHeadH1)
	if len(events) != 1 {
		t.Fatalf("the settle published %d events of k1 at H1, want one", len(events))
	}
	got := events[0].Payload
	artifact, check, state := rig.verification, "k1", "no"
	want := model.CheckStateChangedEventPayload{
		Issue: rig.issue, CheckArtifactID: &artifact, Repo: ciJobRepository, Head: eachHeadH1,
		PullRequests: []int{70}, Checks: []string{"k1", "k2"}, Check: &check, CheckVersion: &version,
		State: &state, Reason: "fail", Roles: []string{}, Results: []string{fail}, CICase: "", Serial: 1,
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("the payload of k1 at H1 is\n%s\nwant\n%s", gotJSON, wantJSON)
	}
	parseWithContractsSchema(t, events[0].Raw)
}

// parseWithContractsSchema parses payload, as the event log stores it, with @legion/contracts'
// CheckStateChangedEventPayloadSchema, through bun, and fails the test where the schema refuses
// it. bun is required: every CI job that runs this package sets it up.
func parseWithContractsSchema(t *testing.T, payload []byte) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatalf("bun is not on PATH, and the stored payload must be parsed with @legion/contracts' schema: %v", err)
	}
	file := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(file, payload, 0o600); err != nil {
		t.Fatalf("write the payload: %v", err)
	}
	contracts, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "contracts"))
	if err != nil {
		t.Fatalf("find packages/contracts: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), bun, "-e", `import { CheckStateChangedEventPayloadSchema } from "./src/index.ts";
const payload = JSON.parse(await Bun.file(process.env.CHECK_STATE_PAYLOAD).text());
CheckStateChangedEventPayloadSchema.parse(payload);
console.log("parsed");`)
	cmd.Dir = contracts
	cmd.Env = append(os.Environ(), "CHECK_STATE_PAYLOAD="+file)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "parsed") {
		t.Fatalf("CheckStateChangedEventPayloadSchema refuses the stored payload %s: %v\n%s", payload, err, out)
	}
}

// TestEachKindOfChangePublishesTheNewState: each kind of write that can change a state publishes
// it, each in a rig of its own: a result recorded, a result retracted, the words of a check moved
// by a document version, a head moved by delivery.UpsertPullRequestHead, a CI run the stand-in for
// GitHub concluded, whose settlement marks the issue with check_states_owe, a check removed, which
// publishes the head's new set of checks in the set form, and a read of GitHub that failed, which
// publishes unknown for read_failed and keeps one mark for a later read.
func TestEachKindOfChangePublishesTheNewState(t *testing.T) {
	t.Run("recorded", func(t *testing.T) {
		rig := newCheckStatesRig(t, checkStatesTwoChecks)
		writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
		settleOwed(t, rig)
		eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "fail", eachHeadH1)
		wantEvents(t, "a recorded fail", settleOwed(t, rig), stateWant{"k1", eachHeadH1, "no", "fail"})
	})
	t.Run("retracted", func(t *testing.T) {
		rig := newCheckStatesRig(t, checkStatesTwoChecks)
		writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
		fail := eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "fail", eachHeadH1)
		settleOwed(t, rig)
		if retracted := retractAsPerson(t, rig.handler, fail, "The run was against the wrong build."); retracted.Code != http.StatusCreated {
			t.Fatalf("retract the fail: status=%d body=%s", retracted.Code, retracted.Body.String())
		}
		wantEvents(t, "the fail retracted", settleOwed(t, rig), stateWant{"k1", eachHeadH1, "no", "no_result"})
	})
	t.Run("words moved through a document version", func(t *testing.T) {
		rig := newCheckStatesRig(t, checkStatesTwoChecks)
		writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
		writeHead(t, rig.database, ciJobRepository, 71, eachHeadH3, "open", rig.issue)
		eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
		eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH3)
		settleOwed(t, rig)
		before := checkOf(t, readResults(t, rig.handler, rig.issue), "k1").Version
		uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue, "verification.md",
			checkStatesBlock("k1", ciCheckOrdinary, "The server records a result against a check, named by its block id.")+"\n"+
				checkStatesBlock("k2", ciCheckOrdinary, "The server answers a read of the results."))
		version := checkOf(t, readResults(t, rig.handler, rig.issue), "k1").Version
		if version == before {
			t.Fatalf("the new words left k1 at version %d", version)
		}
		events := settleOwed(t, rig)
		wantEventSet(t, "k1's words moved", events, stateWant{"k1", eachHeadH1, "no", "no_result"}, stateWant{"k1", eachHeadH3, "no", "no_result"})
		for _, event := range events {
			if event.Payload.CheckVersion == nil || *event.Payload.CheckVersion != version {
				t.Fatalf("the event %s names version %v of k1, want %d", event.line(), event.Payload.CheckVersion, version)
			}
		}
	})
	t.Run("head moved through delivery.UpsertPullRequestHead", func(t *testing.T) {
		rig := newCheckStatesRig(t, checkStatesTwoChecks)
		upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
		settleOwed(t, rig)
		upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
		wantEventSet(t, "a push that moves #70 to H2", settleOwed(t, rig),
			stateWant{"k1", eachHeadH2, "no", "no_result"}, stateWant{"k2", eachHeadH2, "no", "no_result"})
		// H1 is a former head of #70, which is still open, and its tester may have tested H1 while
		// #70 moved on: the settler keeps H1's states, and publishes nothing new there, since no
		// state of H1 changed (TestAFormerHeadOfAnOpenPullRequestKeepsItsStates).
		if _, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH1, "k1"); !held {
			t.Fatal("the state of k1 at H1, a former head of #70, which is still open, is no longer published")
		}
	})
	t.Run("CI settled through check_states_owe", func(t *testing.T) {
		rig := newCheckStatesRig(t, checkStatesWithCI)
		rig.github.listings.set(eachHeadH1, pendingRun(901, ciJobName, "in_progress"))
		writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
		started := eventsOfCheck(settleOwed(t, rig), "k3", eachHeadH1)
		if len(started) != 1 || started[0].state() != "no" || started[0].Payload.CICase != ciCaseNotCompleted {
			t.Fatalf("k3 at H1 while its run goes on: %v, want one event of no with ci_case %s", eventLines(started), ciCaseNotCompleted)
		}
		rig.github.listings.set(eachHeadH1, completedRun(901, ciJobName, "success"))
		if _, err := rig.database.Pool.Exec(context.Background(), `select check_states_owe($1)`, rig.issue); err != nil {
			t.Fatalf("mark %s owed as the settlement of its pull request's checks does: %v", rig.issue, err)
		}
		events := settleOwed(t, rig)
		wantEvents(t, "k3's run concluded", events, stateWant{"k3", eachHeadH1, "yes", ""})
		if !slices.Equal(events[0].Payload.Roles, []string{"ci"}) || events[0].Payload.CICase != "success" {
			t.Fatalf("k3 at H1 once its run passed: roles %v, ci_case %q, want [ci] and success", events[0].Payload.Roles, events[0].Payload.CICase)
		}
	})
	t.Run("a removed check", func(t *testing.T) {
		rig := newCheckStatesRig(t, checkStatesTwoChecks)
		writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
		settleOwed(t, rig)
		uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue, "verification.md",
			checkStatesBlock("k1", ciCheckOrdinary, "The server records a result against a check."))
		events := settleOwed(t, rig)
		wantEvents(t, "k2 removed", events, stateWant{"", eachHeadH1, "", ""})
		if !slices.Equal(events[0].Payload.Checks, []string{"k1"}) {
			t.Fatalf("the set form names the checks %v, want [k1]", events[0].Payload.Checks)
		}
		if _, held := publishedRow(t, rig.database, rig.issue, ciJobRepository, eachHeadH1, "k2"); held {
			t.Fatal("the state of k2, which the document no longer holds, is still published")
		}
	})
	t.Run("a read that failed", func(t *testing.T) {
		rig := newCheckStatesRig(t, checkStatesWithCI)
		rig.github.listings.failWith(http.StatusServiceUnavailable)
		writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
		events := eventsOfCheck(settleOwed(t, rig), "k3", eachHeadH1)
		wantEvents(t, "GitHub answering 503", events, stateWant{"k3", eachHeadH1, "unknown", "read_failed"})
		var due float64
		var failures, marks int
		if err := rig.database.Pool.QueryRow(context.Background(), `
			select count(*), coalesce(max(failures), 0), coalesce(extract(epoch from max(due_at) - now()), 0)::float8
			from check_states_owed where issue_key = $1`, rig.issue).Scan(&marks, &failures, &due); err != nil {
			t.Fatalf("read the marks of %s: %v", rig.issue, err)
		}
		if marks != 1 || failures != 1 || due < 20 || due > 30 {
			t.Fatalf("after the failed read %s holds %d marks, failures %d, due in %.1fs; want one mark, failures 1, due 30s on", rig.issue, marks, failures, due)
		}
	})
}

// upsertHead writes #number of the rig's repository at sha, open and attributed to issue, onto
// main at ciJobBaseSHA, through the writer every intake and reconcile write goes through.
func upsertHead(t *testing.T, rig *checkStatesRig, number int, sha, issue, title string) {
	t.Helper()
	attributed := issue
	if err := rig.served.deps.Attributor.UpsertPullRequestHead(context.Background(), rig.database.Pool, nil, delivery.PullRequestHead{
		Repo: ciJobRepository, Number: number, HeadSHA: sha, HeadRef: "legion/work", BaseRef: "main", BaseSHA: ciJobBaseSHA, State: "open",
		IssueKey: &attributed, AttributionSource: "title", AttributionComplete: true, Title: title,
		URL: fmt.Sprintf("https://github.com/%s/pull/%d", ciJobRepository, number),
	}); err != nil {
		t.Fatalf("write #%d at %s: %v", number, sha, err)
	}
}

// TestOneEventForEachChangeOfAStateAndNoneWhenAResultChangesNoState: the settler settles after
// each write. The implementer's pass of k1 at H1 publishes one event; a second pass of the
// implementer, which leaves the state yes from the implementer, publishes none; the tester's pass
// changes the roles and publishes one; a fail of k2 publishes one, and a second fail of k2 none.
func TestOneEventForEachChangeOfAStateAndNoneWhenAResultChangesNoState(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
	settleOwed(t, rig)
	var counts []int
	for _, write := range []struct{ check, role, verdict string }{
		{"k1", "implementer", "pass"}, {"k1", "implementer", "pass"}, {"k1", "tester", "pass"},
		{"k2", "tester", "fail"}, {"k2", "tester", "fail"},
	} {
		eachHeadResult(t, rig.handler, rig.issue, write.check, write.role, write.verdict, eachHeadH1)
		counts = append(counts, len(settleOwed(t, rig)))
	}
	if want := []int{1, 0, 1, 1, 0}; !slices.Equal(counts, want) {
		t.Fatalf("the writes published %v events, want %v", counts, want)
	}
}

// TestAHeadWithNoCheckGetsTheSetForm: ACME-2 has no Verification document and #80 open at H5. One
// settle publishes one event, the set form, naming no check and no document, and the stored set
// row holds no document either. A second write to #80 that moves no head publishes nothing. An
// issue whose Verification document holds only an after_merge check gets the same set form, naming
// that document.
func TestAHeadWithNoCheckGetsTheSetForm(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	settleOwed(t, rig)
	second := rig.fileIssue(t, "With no Verification document")
	writeHead(t, rig.database, ciJobRepository, 80, checkStatesH5, "open", second)
	events := settleOwed(t, rig)
	wantEvents(t, "a head of an issue with no check", events, stateWant{"", checkStatesH5, "", ""})
	set := events[0].Payload
	if events[0].Issue != second || set.Issue != second || set.CheckArtifactID != nil || len(set.Checks) != 0 ||
		set.CheckVersion != nil || !slices.Equal(set.PullRequests, []int{80}) || set.Repo != ciJobRepository {
		t.Fatalf("the set form of %s is %s, want no document, no check, at %s of #80", second, events[0].Raw, checkStatesH5)
	}
	if !strings.Contains(string(events[0].Raw), `"checks": []`) && !strings.Contains(string(events[0].Raw), `"checks":[]`) {
		t.Fatalf("the stored set form %s does not write checks as an empty list", events[0].Raw)
	}
	row, held := publishedRow(t, rig.database, second, ciJobRepository, checkStatesH5, "")
	if !held || row.checkArtifactID != nil || row.state != nil {
		t.Fatalf("the stored set row of %s at %s is %+v (held %v), want one with no document and no state", second, checkStatesH5, row, held)
	}

	upsertHead(t, rig, 80, checkStatesH5, second, "A new title")
	if _, err := rig.database.Pool.Exec(context.Background(), `select check_states_owe($1)`, second); err != nil {
		t.Fatalf("mark %s owed: %v", second, err)
	}
	if events := settleOwed(t, rig); len(events) != 0 {
		t.Fatalf("a write to #80 that moves no head published %v", eventLines(events))
	}

	third := rig.fileIssue(t, "With an after_merge check alone")
	document := uploadDocument(t, rig.handler, "/api/v1/issues/"+third, "verification.md",
		":::check{#k9 when=\"after_merge\"}\nProduction answers at the merged commit.\n:::\n").Artifact.ID
	writeHead(t, rig.database, ciJobRepository, 81, checkStatesH6, "open", third)
	events = settleOwed(t, rig)
	wantEvents(t, "a head of an issue with an after_merge check alone", events, stateWant{"", checkStatesH6, "", ""})
	if got := events[0].Payload; got.CheckArtifactID == nil || *got.CheckArtifactID != document || len(got.Checks) != 0 {
		t.Fatalf("the set form of %s is %s, want document %s and no check", third, events[0].Raw, document)
	}
}

// TestAHeadOfEachRepositoryTakesItsOwnComputedState: two repositories each hold an open #70 at
// one commit, and the issue resolves to no repository, so neither head's basis names a
// repository (headBasis). k3 is a check CI proves: its read at acme/one finds the run going on,
// and its read at acme/two failed. Each head's state is the subject of its own repository, so
// acme/two reads unknown for read_failed and keeps a mark for a later read, in either order of
// the heads; k1, an ordinary check, reads the commit's one state at both.
func TestAHeadOfEachRepositoryTakesItsOwnComputedState(t *testing.T) {
	for _, order := range [][]string{{"acme/one", "acme/two"}, {"acme/two", "acme/one"}} {
		t.Run(strings.Join(order, " before "), func(t *testing.T) {
			var heads []delivery.AttributedHead
			for _, repo := range order {
				heads = append(heads, delivery.AttributedHead{Repo: repo, Number: 70, HeadSHA: eachHeadH1, Open: true})
			}
			_, subjects := pullRequestBases(heads, nil, true, true)
			reads := map[ciHeadKey]*model.CheckCI{
				{repo: "acme/one", commit: eachHeadH1}: {Repo: "acme/one", Commit: eachHeadH1, State: model.CIStateNoResult, Case: ciCaseNotCompleted},
				{repo: "acme/two", commit: eachHeadH1}: {Repo: "acme/two", Commit: eachHeadH1, State: model.CIStateUnread, Case: ciCaseUnreadable},
			}
			ordinary := model.CheckBlock{BlockID: "k1", When: docs.CheckWhenBeforeMerge}
			ordinary.PassStands = eachSubjectStands(ordinary, comparison{subjects: subjects}, nil)
			proved := model.CheckBlock{BlockID: "k3", When: docs.CheckWhenBeforeMerge}
			proved.PassStands = eachSubjectStands(proved, comparison{subjects: subjects, ci: reads}, nil)
			computed, readFailed, err := checkStatesAtHeads("ACME-1", heads, nil, issueCheckResults{Checks: []model.CheckBlock{ordinary, proved}})
			if err != nil {
				t.Fatalf("compute the states at the two heads: %v", err)
			}
			got := map[string]string{}
			for _, head := range computed {
				for _, state := range head.states {
					got[head.repo+" "+state.check] = state.state + "/" + state.reason + "/" + state.ciCase
				}
			}
			want := map[string]string{
				"acme/one k1": "no/no_result/", "acme/two k1": "no/no_result/",
				"acme/one k3": "no/no_result/" + ciCaseNotCompleted, "acme/two k3": "unknown/read_failed/" + ciCaseUnreadable,
			}
			if !maps.Equal(got, want) || !readFailed {
				t.Fatalf("the states are %v with readFailed %v, want %v and true", got, readFailed, want)
			}
		})
	}
}
