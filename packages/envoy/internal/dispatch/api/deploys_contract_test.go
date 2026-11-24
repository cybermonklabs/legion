package api

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestADeployRecordNamesItsWriter(t *testing.T) {
	t.Parallel()
	for _, actor := range []model.Actor{
		{Kind: "user", ID: "alice", Proven: true},
		{Kind: "session", ID: "personal", Owner: new("alice"), Credential: &model.Credential{TokenID: "token"}},
		{Kind: "session", ID: "pod", Service: new("system:serviceaccount:acme:worker")},
		{Credential: &model.Credential{}},
		{Kind: "session", ID: "broker", Owner: new("alice"), Proven: true, Credential: &model.Credential{Session: "broker", Scopes: []string{model.ScopePerson}}},
		{Kind: "session", ID: "job", Service: new("acme/release/.github/workflows/deploy.yml@refs/heads/main"), Proven: true},
	} {
		if got, want := deployWriterOf(actor), actor.Recorded(); !reflect.DeepEqual(got, want) {
			t.Errorf("writer for %+v = %+v, want %+v", actor, got, want)
		}
	}
}

func TestALiveCheckCountsOnlyTheDeployOfTheProgramItNames(t *testing.T) {
	rig := newAfterMergeRig(t)
	restore := useTheNextReleasesManifest(t)
	issue := rig.issue(t, "A named program", ":::check{#live when=\"after_merge\" subject=\"production\" program=\"worker\"}\nThe worker answers.\n:::\n")
	unnamed := rig.issue(t, "An unnamed program", ":::check{#live when=\"after_merge\" subject=\"production\"}\nThe program answers.\n:::\n")
	restore()
	for index, key := range []string{issue, unnamed} {
		writeKnownMergeOnBase(t, rig.database, "acme/widgets", 200+index, headCommit, key, "main", afterMergeM)
	}
	rig.github.carry("main", afterMergeM, afterMergeC1, afterMergeC2, afterMergeC0)
	rig.github.hold(afterMergeM, afterMergeC1, afterMergeC2, afterMergeC0)
	postDeploy(t, rig.handler, deployBody("renderer", afterMergeC1), http.StatusCreated)
	worker := postDeploy(t, rig.handler, deployBody("worker", afterMergeC2), http.StatusCreated)
	wrong := rig.record(t, issue, "live", "tester", "pass", afterMergeC1)
	read := readLiveCheck(t, rig.handler, issue, "")
	requireLiveDecision(t, "another program's commit", read, "unknown", "no_result_from_deploy", "", &worker)
	requireNotCounted(t, read, wrong, "it names a commit that the current deploy of the program this check reads does not run")
	rig.record(t, issue, "live", "tester", "pass", afterMergeC2)
	requireLiveDecision(t, "the named program's result", readLiveCheck(t, rig.handler, issue, ""), "yes", "", afterMergeC2, &worker)
	postDeploy(t, rig.handler, deployBody("renderer", afterMergeC0), http.StatusCreated)
	requireLiveDecision(t, "another program's deploy changes nothing", readLiveCheck(t, rig.handler, issue, ""), "yes", "", afterMergeC2, &worker)
	rig.record(t, unnamed, "live", "tester", "pass", afterMergeC2)
	read = readLiveCheck(t, rig.handler, unnamed, "")
	requireLiveDecision(t, "two programs and no name", read, "unknown", "program_not_named", "", nil)
	if read.PassStands.Deploy != nil {
		t.Fatalf("an unnamed program chose a deploy: %+v", read.PassStands.Deploy)
	}
	fail := rig.record(t, issue, "live", "reviewer", "fail", afterMergeC2)
	read = readLiveCheck(t, rig.handler, issue, "")
	requireLiveDecision(t, "the newest live result is a fail", read, "no", "fail", afterMergeC2, &worker)
	if !slices.Equal(read.PassStands.Results, []string{fail}) {
		t.Fatalf("the live fail is decided by %v, want %s", read.PassStands.Results, fail)
	}
	pass := rig.record(t, issue, "live", "tester", "pass", afterMergeC2)
	read = readLiveCheck(t, rig.handler, issue, "")
	requireLiveDecision(t, "the newest live pass ends the failure", read, "yes", "", afterMergeC2, &worker)
	if !slices.Equal(read.PassStands.Results, []string{pass}) {
		t.Fatalf("the live pass is decided by %v, want %s", read.PassStands.Results, pass)
	}
}

func TestAnUnnamedLiveCheckReadsTheOnlyProgram(t *testing.T) {
	rig := newAfterMergeRig(t)
	restore := useTheNextReleasesManifest(t)
	issue := rig.issue(t, "One program", ":::check{#live when=\"after_merge\" subject=\"graph\"}\nThe graph answers.\n:::\n")
	restore()
	writeKnownMergeOnBase(t, rig.database, "acme/widgets", 300, headCommit, issue, "main", afterMergeM)
	rig.github.carry("main", afterMergeM, afterMergeC1)
	rig.github.hold(afterMergeM, afterMergeC1)
	deploy := postDeploy(t, rig.handler, deployBody("worker", afterMergeC1), http.StatusCreated)
	rig.record(t, issue, "live", "tester", "pass", afterMergeC1)
	requireLiveDecision(t, "the only program is unambiguous", readLiveCheck(t, rig.handler, issue, ""), "yes", "", afterMergeC1, &deploy)
}

func TestARecordedDeployWakesEachOpenIssueThatHoldsALiveResult(t *testing.T) {
	rig := newAfterMergeRig(t)
	restore := useTheNextReleasesManifest(t)
	var issues []string
	for index, attributes := range []string{
		`subject="production"`, `subject="code"`, `subject="graph" program="renderer"`,
		`subject="production"`, `subject="production"`, `subject="graph" program="worker"`,
	} {
		issues = append(issues, rig.issue(t, fmt.Sprintf("Event recipient %d", index+1), ":::check{#live when=\"after_merge\" "+attributes+"}\nThe system answers.\n:::\n"))
	}
	restore()
	for index, issue := range issues {
		if index != 4 {
			rig.record(t, issue, "live", "tester", "pass", afterMergeC1)
		}
	}
	if _, err := rig.database.Pool.Exec(context.Background(), `update issues set status = 'done', closed_at = now() where key = $1`, issues[3]); err != nil {
		t.Fatal(err)
	}
	postDeploy(t, rig.handler, deployBody("renderer", afterMergeC1), http.StatusCreated)
	rows, err := rig.database.Pool.Query(context.Background(), `select issue_key from events where type = 'deploy.recorded' order by issue_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var recipients []string
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			t.Fatal(err)
		}
		recipients = append(recipients, issue)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(recipients, []string{issues[0], issues[2]}) {
		t.Fatalf("deploy recipients = %v, want only %s and %s", recipients, issues[0], issues[2])
	}
}
