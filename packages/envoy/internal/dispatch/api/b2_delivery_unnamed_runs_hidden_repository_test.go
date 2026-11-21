package api

import (
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// mapDeployRepositoryToPRIV makes PRIV an owner of the deploy repository, acme/widgets
// (refs.ProjectRepositoriesSQL): PRIV's issue names it.
func mapDeployRepositoryToPRIV(t *testing.T, seed privateSeed) {
	t.Helper()
	if _, err := seed.database.Pool.Exec(t.Context(), `update issues set repo = 'acme/widgets' where key = $1`, seed.issue[seedPrivate]); err != nil {
		t.Fatalf("name acme/widgets on PRIV's issue: %v", err)
	}
}

// runAnswer is caller's GET /api/v1/delivery/runs/{id}, as status and body with the id written
// <id>, so a run's answer compares with the answer of an id no run holds.
func runAnswer(t *testing.T, seed privateSeed, caller privateCaller, id int64) string {
	t.Helper()
	response := seed.request(t, caller, http.MethodGet, "/api/v1/delivery/runs/"+strconv.FormatInt(id, 10), nil)
	return fmt.Sprintf("%d %s", response.Code, strings.ReplaceAll(strings.TrimSpace(response.Body.String()), strconv.FormatInt(id, 10), "<id>"))
}

// TestADeployRunThatShippedNothingIsHiddenWhereAHiddenProjectOwnsTheDeployRepository: a deploy
// run that shipped no pull request is shown to an open viewer only when the deploy repository is
// not hidden from that viewer (refs.RepositoryClause), as a pull request that names no issue is. Over
// seedRunThatShippedNothing's seed with the failed run 702, with PRIV, private, mapping the deploy
// repository acme/widgets, alice, who does not see PRIV, is shown run 702 in none of her reads: her
// timeline lists no run (R1 shipped only PRIV's P2), her measures count no concluded deploy run,
// her Pipeline page counts no production apply, and her read of run 702 answers what run 999, which
// no run holds, answers. carol, who sees PRIV, still reads run 702's jobs.
func TestADeployRunThatShippedNothingIsHiddenWhereAHiddenProjectOwnsTheDeployRepository(t *testing.T) {
	seed := seedRunThatShippedNothing(t, true)
	mapDeployRepositoryToPRIV(t, seed)
	if got := readShownDelivery(t, seed, callerAlice).runs; len(got) != 0 {
		t.Errorf("alice reads the timeline's runs as %v, want none: run 702 shipped nothing in a repository PRIV owns", got)
	}
	if got := decodeBody[deliveryMeasuresResponse](t, seed.mustRequest(t, callerAlice, http.MethodGet, deliveryReadsTargets[1].target, nil, http.StatusOK)).Measures.DeployRunSuccess.Concluded; got != 0 {
		t.Errorf("alice's deploy_run_success counts %d concluded runs, want none", got)
	}
	if got := decodeBody[deliveryPipelineResponse](t, seed.mustRequest(t, callerAlice, http.MethodGet, deliveryReadsTargets[2].target, nil, http.StatusOK)).ProductionApply.Total.N; got != 0 {
		t.Errorf("alice's production_apply counts %d deploys, want none", got)
	}
	missing := runAnswer(t, seed, callerAlice, 999)
	if !strings.HasPrefix(missing, "404 ") {
		t.Fatalf("alice reads run 999, which no run holds, as %s: the comparison proves nothing", missing)
	}
	if got := runAnswer(t, seed, callerAlice, deliveryRunFailed); got != missing {
		t.Errorf("alice reads run 702 as %s, want what run 999 answers: %s", got, missing)
	}
	if got := runAnswer(t, seed, callerCarol, deliveryRunFailed); !strings.HasPrefix(got, "200 ") {
		t.Errorf("carol, who sees PRIV, reads run 702 as %s, want its jobs", got)
	}
}

// TestAPRChecksRunNamingNoStoredPullRequestIsHiddenWhereAHiddenProjectOwnsTheDeployRepository: a
// PR-checks run whose number no stored pull request holds names no issue, and is shown to an open
// viewer only when the deploy repository is not hidden from that viewer. Over seedDeliveryReads'
// whole seed with PRIV, private, mapping acme/widgets, alice's Pipeline page counts no PR-checks run
// (C2 is PRIV's P2's and C9 names #99, which no row holds), and her read of C9 answers what run 999
// answers. carol, who sees PRIV, still counts both and reads C9.
func TestAPRChecksRunNamingNoStoredPullRequestIsHiddenWhereAHiddenProjectOwnsTheDeployRepository(t *testing.T) {
	seed := seedDeliveryReads(t, true, true)
	mapDeployRepositoryToPRIV(t, seed)
	if got := decodeBody[deliveryPipelineResponse](t, seed.mustRequest(t, callerAlice, http.MethodGet, deliveryReadsTargets[2].target, nil, http.StatusOK)).PRChecks.Run.Total.N; got != 0 {
		t.Errorf("alice's pr_checks count %d runs, want none: C9 names no stored pull request in a repository PRIV owns", got)
	}
	missing := runAnswer(t, seed, callerAlice, 999)
	if !strings.HasPrefix(missing, "404 ") {
		t.Fatalf("alice reads run 999, which no run holds, as %s: the comparison proves nothing", missing)
	}
	if got := runAnswer(t, seed, callerAlice, deliveryRunC9); got != missing {
		t.Errorf("alice reads C9 as %s, want what run 999 answers: %s", got, missing)
	}
	if got := decodeBody[deliveryPipelineResponse](t, seed.mustRequest(t, callerCarol, http.MethodGet, deliveryReadsTargets[2].target, nil, http.StatusOK)).PRChecks.Run.Total.N; got != 2 {
		t.Errorf("carol's pr_checks count %d runs, want C2 and C9", got)
	}
	if got := runAnswer(t, seed, callerCarol, deliveryRunC9); !strings.HasPrefix(got, "200 ") {
		t.Errorf("carol, who sees PRIV, reads C9 as %s, want its jobs", got)
	}
}

// TestRunsNamingNoPullRequestAreShownAsBeforeWhereNoProjectIsPrivate (R9): with every project
// public, the mapping of acme/widgets hides nothing: alice is shown run 702, which shipped nothing,
// in her timeline, measures and Pipeline page and reads its jobs, and her Pipeline page counts C2
// and C9 and she reads C9's jobs, as before any project could be private.
func TestRunsNamingNoPullRequestAreShownAsBeforeWhereNoProjectIsPrivate(t *testing.T) {
	failed := seedRunThatShippedNothing(t, true)
	checks := seedDeliveryReads(t, true, true)
	for _, seed := range []privateSeed{failed, checks} {
		mapDeployRepositoryToPRIV(t, seed)
		if _, err := seed.database.Pool.Exec(t.Context(), `update projects set visibility = 'public'`); err != nil {
			t.Fatalf("make every project public: %v", err)
		}
	}
	if got := readShownDelivery(t, failed, callerAlice).runs; !reflect.DeepEqual(got[deliveryRunFailed], []string{}) {
		t.Errorf("alice reads the timeline's runs as %v, want run 702 among them, with no pull request", got)
	}
	if got := decodeBody[deliveryMeasuresResponse](t, failed.mustRequest(t, callerAlice, http.MethodGet, deliveryReadsTargets[1].target, nil, http.StatusOK)).Measures.DeployRunSuccess.Concluded; got != 2 {
		t.Errorf("alice's deploy_run_success counts %d concluded runs, want R1 and run 702", got)
	}
	if got := decodeBody[deliveryPipelineResponse](t, failed.mustRequest(t, callerAlice, http.MethodGet, deliveryReadsTargets[2].target, nil, http.StatusOK)).ProductionApply.Total.N; got != 2 {
		t.Errorf("alice's production_apply counts %d deploys, want R1 and run 702", got)
	}
	if got := decodeBody[deliveryPipelineResponse](t, checks.mustRequest(t, callerAlice, http.MethodGet, deliveryReadsTargets[2].target, nil, http.StatusOK)).PRChecks.Run.Total.N; got != 2 {
		t.Errorf("alice's pr_checks count %d runs, want C2 and C9", got)
	}
	for _, read := range []struct {
		seed privateSeed
		run  int64
	}{{failed, deliveryRunFailed}, {checks, deliveryRunC9}} {
		response := read.seed.request(t, callerAlice, http.MethodGet, "/api/v1/delivery/runs/"+strconv.FormatInt(read.run, 10), nil)
		if response.Code != http.StatusOK || len(decodeBody[delivery.DeliveryRunDetailView](t, response).Jobs) != 1 {
			t.Errorf("alice reads run %d as %d %s, want its one job", read.run, response.Code, response.Body.String())
		}
	}
}
