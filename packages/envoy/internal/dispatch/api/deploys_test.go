package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

const afterMergeC2 = newHeadCommit

type observedDeploy struct {
	ID         int64     `json:"id"`
	Repo       string    `json:"repo"`
	Program    string    `json:"program"`
	Commit     string    `json:"commit"`
	DeployedAt time.Time `json:"deployed_at"`
	Source     string    `json:"source"`
	RecordedBy struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"recorded_by"`
}

func postDeploy(t *testing.T, handler http.Handler, body map[string]any, status int) observedDeploy {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/deploys", body, "alice")
	if response.Code != status {
		t.Fatalf("record a deploy: HTTP %d, want %d: %s", response.Code, status, response.Body)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return observedDeploy{}
	}
	return decodeBody[struct {
		Deploy observedDeploy `json:"deploy"`
	}](t, response).Deploy
}

func deployBody(program, commit string) map[string]any {
	return map[string]any{"repo": "acme/widgets", "program": program, "commit": commit}
}

func TestTheDeployRoutesRecordAndReadEachProgram(t *testing.T) {
	t.Parallel()
	rig := newAfterMergeRig(t)
	first := postDeploy(t, rig.handler, deployBody("renderer", afterMergeC1), http.StatusCreated)
	if first.ID == 0 || first.Repo != "acme/widgets" || first.Program != "renderer" || first.Commit != afterMergeC1 ||
		first.DeployedAt.IsZero() || first.Source != "route" || first.RecordedBy.Kind != "user" || first.RecordedBy.ID != "alice" {
		t.Fatalf("the recorded deploy is %+v, want the program, commit, server time and resolved caller", first)
	}
	later := deployBody("renderer", afterMergeC1)
	later["deployed_at"] = first.DeployedAt.Add(time.Minute)
	same := postDeploy(t, rig.handler, later, http.StatusOK)
	if same.ID != first.ID || !same.DeployedAt.Equal(first.DeployedAt) {
		t.Fatalf("recording the current commit moved its deploy: first %+v, repeated %+v", first, same)
	}
	older := deployBody("renderer", afterMergeC2)
	older["deployed_at"] = first.DeployedAt.Add(-time.Minute)
	refused := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/deploys", older, "alice")
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), "DEPLOY_NOT_NEWEST") ||
		!strings.Contains(refused.Body.String(), first.Commit) {
		t.Fatalf("an older deploy answered %d: %s, want DEPLOY_NOT_NEWEST and the current commit", refused.Code, refused.Body)
	}
	worker := postDeploy(t, rig.handler, deployBody("worker", afterMergeC2), http.StatusCreated)
	response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/deploys?repo=acme%2Fwidgets", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read current deploys: %d %s", response.Code, response.Body)
	}
	read := decodeBody[struct {
		Repo    string           `json:"repo"`
		Deploys []observedDeploy `json:"deploys"`
	}](t, response)
	if read.Repo != "acme/widgets" || len(read.Deploys) != 2 || read.Deploys[0].ID != first.ID || read.Deploys[1].ID != worker.ID {
		t.Fatalf("current deploys are %+v, want renderer and worker in program order", read)
	}
	var count int
	if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from deploys`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("the deploy table holds %d rows, want 2; read error %v", count, err)
	}
}

func TestADeployRouteRefusesBadFieldsBeforeWriting(t *testing.T) {
	t.Parallel()
	rig := newAfterMergeRig(t)
	for _, test := range []struct {
		field string
		value any
	}{
		{"repo", "not-a-repository"}, {"program", "legion-ACME"}, {"program", ""},
		{"program", strings.Repeat("a", 65)}, {"commit", "not-a-commit"},
		{"commit", strings.Repeat("A", 40)}, {"deployed_at", time.Now().UTC().Add(6 * time.Minute)},
	} {
		t.Run(test.field+fmt.Sprint(test.value), func(t *testing.T) {
			body := deployBody("renderer", afterMergeC1)
			body[test.field] = test.value
			response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/deploys", body, "alice")
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "DEPLOY_INPUT") {
				t.Fatalf("bad %s answered %d: %s, want DEPLOY_INPUT", test.field, response.Code, response.Body)
			}
		})
	}
	var count int
	if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from deploys`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("bad requests left %d deploys, want none; read error %v", count, err)
	}
}

type liveCheckAnswer struct {
	BlockID    string `json:"block_id"`
	PassStands struct {
		Answer  string          `json:"answer"`
		Reason  string          `json:"reason"`
		Commit  string          `json:"commit"`
		Results []string        `json:"results"`
		Deploy  *observedDeploy `json:"deploy"`
	} `json:"pass_stands"`
	Results []struct {
		ID         string `json:"id"`
		NotCounted string `json:"not_counted"`
	} `json:"results"`
}

func readLiveCheck(t *testing.T, handler http.Handler, issue, query string) liveCheckAnswer {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/results"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read live results: %d %s", response.Code, response.Body)
	}
	read := decodeBody[struct {
		Checks []liveCheckAnswer `json:"checks"`
	}](t, response)
	for _, check := range read.Checks {
		if check.BlockID == "live" {
			return check
		}
	}
	t.Fatal("the results route omitted the live check")
	return liveCheckAnswer{}
}

func requireLiveDecision(t *testing.T, what string, got liveCheckAnswer, answer, reason, commit string, deploy *observedDeploy) {
	t.Helper()
	if got.PassStands.Answer != answer || got.PassStands.Reason != reason || got.PassStands.Commit != commit {
		t.Fatalf("%s: pass_stands %+v, want %s/%s at %s", what, got.PassStands, answer, reason, commit)
	}
	if deploy != nil && (got.PassStands.Deploy == nil || got.PassStands.Deploy.Program != deploy.Program ||
		got.PassStands.Deploy.Commit != deploy.Commit || !got.PassStands.Deploy.DeployedAt.Equal(deploy.DeployedAt)) {
		t.Fatalf("%s: deploy %+v, want program %s at %s since %s", what, got.PassStands.Deploy, deploy.Program, deploy.Commit, deploy.DeployedAt)
	}
}

func requireNotCounted(t *testing.T, check liveCheckAnswer, id, reason string) {
	t.Helper()
	for _, result := range check.Results {
		if result.ID == id {
			if result.NotCounted != reason {
				t.Fatalf("result %s not_counted = %q, want %q", id, result.NotCounted, reason)
			}
			return
		}
	}
	t.Fatalf("the route omitted result %s", id)
}

func TestAGraphCheckReadsTheCurrentDeployAsProductionDoes(t *testing.T) {
	// Not parallel: it swaps pmdoc's schema manifest for the whole process (useTheNextReleasesManifest, pmdoc.UseManifestForTest).
	for _, subject := range []string{"production", "graph"} {
		t.Run(subject, func(t *testing.T) {
			rig := newAfterMergeRig(t)
			restore := useTheNextReleasesManifest(t)
			issue := rig.issue(t, "A running program", fmt.Sprintf(":::check{#live when=\"after_merge\" subject=%q program=\"renderer\"}\nThe running program answers.\n:::\n", subject))
			restore()
			writeKnownMergeOnBase(t, rig.database, "acme/widgets", 101, headCommit, issue, "main", afterMergeM)
			rig.github.carry("main", afterMergeM, afterMergeC0, afterMergeC1, afterMergeC2)
			rig.github.hold(afterMergeM, afterMergeC1, afterMergeC2)
			before := rig.record(t, issue, "live", "tester", "pass", afterMergeC1)
			requireLiveDecision(t, "without a recorded deploy", readLiveCheck(t, rig.handler, issue, ""), "unknown", "no_deploy", "", nil)
			first := postDeploy(t, rig.handler, deployBody("renderer", afterMergeC1), http.StatusCreated)
			read := readLiveCheck(t, rig.handler, issue, "")
			requireLiveDecision(t, "a result predating the deploy", read, "unknown", "no_result_from_deploy", "", &first)
			requireNotCounted(t, read, before, "it was recorded before the current deploy of the program this check reads")
			pass := rig.record(t, issue, "live", "tester", "pass", afterMergeC1)
			requireLiveDecision(t, "a result after the deploy", readLiveCheck(t, rig.handler, issue, ""), "yes", "", afterMergeC1, &first)
			later := deployBody("renderer", afterMergeC1)
			later["deployed_at"] = first.DeployedAt.Add(time.Minute)
			same := postDeploy(t, rig.handler, later, http.StatusOK)
			if !same.DeployedAt.Equal(first.DeployedAt) {
				t.Fatalf("an unchanged commit moved deployed_at from %s to %s", first.DeployedAt, same.DeployedAt)
			}
			requireLiveDecision(t, "the same deploy still counts its result", readLiveCheck(t, rig.handler, issue, ""), "yes", "", afterMergeC1, &first)
			second := postDeploy(t, rig.handler, deployBody("renderer", afterMergeC2), http.StatusCreated)
			read = readLiveCheck(t, rig.handler, issue, "")
			requireLiveDecision(t, "a new commit excludes the old deploy", read, "unknown", "no_result_from_deploy", "", &second)
			requireNotCounted(t, read, pass, "it names a commit that the current deploy of the program this check reads does not run")
			rig.record(t, issue, "live", "tester", "pass", afterMergeC2)
			requireLiveDecision(t, "the new deploy has its own result", readLiveCheck(t, rig.handler, issue, ""), "yes", "", afterMergeC2, &second)
			historical := readLiveCheck(t, rig.handler, issue, "?at="+afterMergeC1)
			requireLiveDecision(t, "a named historical read ignores the current deploy", historical, "yes", "", afterMergeC1, nil)
			if historical.PassStands.Deploy != nil {
				t.Fatalf("a named historical read serves a current deploy: %+v", historical.PassStands.Deploy)
			}
			rollback := postDeploy(t, rig.handler, deployBody("renderer", afterMergeC1), http.StatusCreated)
			read = readLiveCheck(t, rig.handler, issue, "")
			requireLiveDecision(t, "a rollback starts a new observation interval", read, "unknown", "no_result_from_deploy", "", &rollback)
			requireNotCounted(t, read, pass, "it was recorded before the current deploy of the program this check reads")
			rig.record(t, issue, "live", "tester", "pass", afterMergeC1)
			requireLiveDecision(t, "the rollback has its own result", readLiveCheck(t, rig.handler, issue, ""), "yes", "", afterMergeC1, &rollback)
		})
	}
}
