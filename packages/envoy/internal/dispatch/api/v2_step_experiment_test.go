package api

// v2_step_experiment_test.go holds what #5014 gave the /api/v1 step records to their /api/v2
// successors: the experiment write, PUT /api/v2/issues/{key}/steps/{id}/experiment, and the
// experiment and the variant GET /api/v2/issues/{key}/steps answers on each run and GET
// /api/v2/projects/{key}/plans on each finished issue. The route tour draws the write's refusals
// too (tourRefusals).

import (
	"fmt"
	"maps"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The /api/v2 experiment write records the experiment and the variant of a run as the /api/v1 one
// does (TestTheExperimentRouteWritesTheVariantOfARun): written after the run's start, which the
// /api/v2 step write made, the pair is what GET /api/v2/issues/{key}/steps answers on the run and,
// once the issue is closed, what GET /api/v2/projects/{key}/plans answers on the issue, as the
// /api/v1 reads answer them. The same pair sent again answers the record, and a second variant
// leaves the first. Each refusal the write answers is one its row declares, and it answers every
// refusal its row declares: an undeclared query key, an issue that does not exist, a body naming
// neither, one naming the variant alone and a name outside the daemon's rule, a writer other than
// the issue's daemon, a run of another issue, and a pair with no identity sent to an id no record
// has. No refused write leaves a record or moves a run's pair.
func TestTheV2ExperimentWriteNamesTheVariantTheV2ReadsAnswer(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of an experiment")
	other := createAdvancingIssue(t, handler, map[string]any{"project": "LEG", "title": "A run of another issue", "force": true}, "alice").Key
	daemon := daemonSessionOf("LEG")
	run, elsewhere := uuid.NewString(), uuid.NewString()
	experimentOf := func(key, id string) string { return "/api/v2/issues/" + key + "/steps/" + id + "/experiment" }
	pair := func(experiment, variant string) map[string]any {
		body := map[string]any{"actor": daemon}
		if experiment != "" {
			body["experiment"] = experiment
		}
		if variant != "" {
			body["variant"] = variant
		}
		return body
	}
	named := func(experiment, variant *string) string {
		if experiment == nil || variant == nil {
			return fmt.Sprintf("%v/%v", experiment, variant)
		}
		return *experiment + "/" + *variant
	}

	for key, id := range map[string]string{issue: run, other: elsewhere} {
		if started := sessionRequest(t, handler, http.MethodPut, "/api/v2/issues/"+key+"/steps/"+id, implementerStart(daemon)); started.Code != http.StatusOK {
			t.Fatalf("start the run %s of %s on /api/v2: status=%d body=%s, want 200", id, key, started.Code, started.Body.String())
		}
	}
	written := sessionRequest(t, handler, http.MethodPut, experimentOf(issue, run), pair("planning", "b"))
	if written.Code != http.StatusOK {
		t.Fatalf("write the variant of %s on /api/v2: status=%d body=%s, want 200", run, written.Code, written.Body.String())
	}
	if answer := decodeBody[model.StepRecord](t, written); answer.ID != run || named(answer.Experiment, answer.Variant) != "planning/b" {
		t.Fatalf("the /api/v2 write answers %s, want the run %s with the variant planning/b", asJSON(t, answer), run)
	}
	requireTheRun := func(when string) {
		t.Helper()
		listed := dispatchRequest(t, handler, http.MethodGet, "/api/v2/issues/"+issue+"/steps", nil, "alice")
		if listed.Code != http.StatusOK {
			t.Fatalf("GET /api/v2/issues/%s/steps %s: status=%d body=%s", issue, when, listed.Code, listed.Body.String())
		}
		steps := decodeBody[page[model.StepRecord]](t, listed).Items
		if len(steps) != 1 || steps[0].ID != run || named(steps[0].Experiment, steps[0].Variant) != "planning/b" || steps[0].StartedAt == nil {
			t.Fatalf("GET /api/v2/issues/%s/steps %s lists %s, want the one run %s, started, with the variant planning/b", issue, when, asJSON(t, steps), run)
		}
		if step := stepOf(t, handler, issue, run); named(step.Experiment, step.Variant) != "planning/b" {
			t.Fatalf("GET /api/v1/issues/%s/steps %s lists the variant %s, want planning/b as /api/v2 answers", issue, when, named(step.Experiment, step.Variant))
		}
	}
	requireTheRun("after the write")

	for _, again := range []struct{ name, variant string }{{"the same pair again", "b"}, {"a second variant", "a"}} {
		response := sessionRequest(t, handler, http.MethodPut, experimentOf(issue, run), pair("planning", again.variant))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s, want 200", again.name, response.Code, response.Body.String())
		}
		if answer := decodeBody[model.StepRecord](t, response); named(answer.Experiment, answer.Variant) != "planning/b" {
			t.Fatalf("%s answers the variant %s, want the first, planning/b", again.name, named(answer.Experiment, answer.Variant))
		}
	}
	requireTheRun("after the pair again and a second variant")

	var declared map[string]bool
	for _, route := range (&server{}).routes() {
		if route.Method == http.MethodPut && route.Pattern == "/api/v2/issues/{key}/steps/{id}/experiment" {
			declared = map[string]bool{}
			for _, refusal := range route.Refusals {
				declared[refusal.Code.Name()] = true
			}
		}
	}
	if declared == nil {
		t.Fatal("the table mounts no PUT /api/v2/issues/{key}/steps/{id}/experiment")
	}
	drawn := map[string]bool{}
	for _, refused := range []struct {
		name, target string
		body         map[string]any
		want         apicode.Code
	}{
		{"an undeclared query key", experimentOf(issue, run) + "?zz_undeclared=1", pair("planning", "c"), apicode.QueryKeyUnknown},
		{"an issue that does not exist", experimentOf("LEG-99999", run), pair("planning", "c"), apicode.IssueNotFound},
		{"a body naming neither", experimentOf(issue, run), pair("", ""), apicode.StepInput},
		{"a body naming the variant alone", experimentOf(issue, run), pair("", "c"), apicode.StepInput},
		{"an experiment outside the daemon's rule for names", experimentOf(issue, run), pair("Planning", "c"), apicode.StepInput},
		{"a writer other than the issue's daemon", experimentOf(issue, run),
			withFields(pair("planning", "c"), map[string]any{"actor": daemonSessionOf("OTHER")}), apicode.StepWriter},
		{"a run of another issue", experimentOf(issue, elsewhere), pair("planning", "c"), apicode.StepOtherIssue},
		{"a pair with no identity to an id no record has", experimentOf(issue, uuid.NewString()), pair("planning", "c"), apicode.StepNotFound},
	} {
		response := sessionRequest(t, handler, http.MethodPut, refused.target, refused.body)
		code, text := refusalOf(t, response)
		if response.Code != refused.want.Status() || code != refused.want.Name() {
			t.Errorf("%s: status=%d code=%s error=%q, want %d %s", refused.name, response.Code, code, text, refused.want.Status(), refused.want.Name())
		}
		if !declared[code] {
			t.Errorf("%s is answered %s, which the route's row does not declare (it declares %v)", refused.name, code, declared)
		}
		drawn[code] = true
	}
	if !maps.Equal(drawn, declared) {
		t.Errorf("the refused writes drew %v, want every refusal the route's row declares, %v", drawn, declared)
	}
	requireTheRun("after the refused writes")
	if step := stepOf(t, handler, other, elsewhere); step.Experiment != nil || step.Variant != nil {
		t.Fatalf("the refused write of a run of %s left it the variant %s, want none", other, named(step.Experiment, step.Variant))
	}

	closeIssue(t, handler, issue)
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v2/projects/LEG/plans", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("GET /api/v2/projects/LEG/plans: status=%d body=%s", listed.Code, listed.Body.String())
	}
	v2 := map[string]string{}
	for _, finished := range decodeBody[page[model.FinishedIssue]](t, listed).Items {
		v2[finished.Issue] = named(finished.Experiment, finished.Variant)
	}
	v1 := map[string]string{}
	for _, finished := range readPlans(t, handler, "LEG", "").Issues {
		v1[finished.Issue] = named(finished.Experiment, finished.Variant)
	}
	if v2[issue] != "planning/b" || !maps.Equal(v2, v1) {
		t.Fatalf("GET /api/v2/projects/LEG/plans names the variants %v, want %s at planning/b as GET /api/v1/projects/LEG/plans names them, %v", v2, issue, v1)
	}
}
