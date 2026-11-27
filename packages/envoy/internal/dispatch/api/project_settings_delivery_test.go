package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// newDeliverySettingsRig is the server under test with Dispatch's GitHub App answering from fake
// (fakeGitHubApp: a repository it lists is one the App can read) and the broker-token check wired
// to a stub broker (newBrokerTokenRigOn).
func newDeliverySettingsRig(t *testing.T, fake *fakeGitHubApp) *brokerTokenRig {
	t.Helper()
	return newDeliverySettingsRigRecording(t, fake, nil)
}

// newDeliverySettingsRigRecording is newDeliverySettingsRig whose GitHub hands each request to
// record, where it is set, before fake answers it (githubtest.Spec.Record).
func newDeliverySettingsRigRecording(t *testing.T, fake *fakeGitHubApp, record func(*http.Request)) *brokerTokenRig {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	github := githubtest.Server(t, githubtest.Spec{Record: record, Handle: fake.handler(t).ServeHTTP})
	return newBrokerTokenRigOn(t, testServerOptions{app: &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText}, githubAPIBase: github})
}

// readableRepos is the repositories the settings tests' GitHub App can read.
func readableRepos() *fakeGitHubApp {
	return &fakeGitHubApp{installations: map[string]string{
		"acme/alpha": "read", "acme/alpha-deploy": "read", "acme/beta": "read", "acme/beta-deploy": "read",
	}}
}

// workflowSource is a github_workflow source of a save's body, naming repo where it is not "".
func workflowSource(repo, workflow, job string) map[string]any {
	source := map[string]any{
		"kind": delivery.SourceKindGitHubWorkflow, "deploy_workflow_path": workflow,
		"production_job_names": []string{job}, "pr_checks_workflow_path": ".github/workflows/ci.yml",
	}
	if repo != "" {
		source["repo"] = repo
	}
	return source
}

// putProjectDelivery sends body as project's delivery settings, as alice.
func putProjectDelivery(t *testing.T, handler http.Handler, project string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/"+project+"/settings/delivery", body, "alice")
}

// savedProjectDelivery is putProjectDelivery that must succeed, and its answer.
func savedProjectDelivery(t *testing.T, handler http.Handler, project string, body map[string]any) projectDeliverySettings {
	t.Helper()
	response := putProjectDelivery(t, handler, project, body)
	if response.Code != http.StatusOK {
		t.Fatalf("save %s's delivery settings: status=%d body=%s", project, response.Code, response.Body.String())
	}
	return decodeBody[projectDeliverySettings](t, response)
}

// readProjectDelivery is GET /api/v1/projects/{key}/settings/delivery as alice: the answer's bytes,
// which are `null` for a project with none.
func readProjectDelivery(t *testing.T, handler http.Handler, project string) []byte {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/"+project+"/settings/delivery", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s's delivery settings: status=%d body=%s", project, response.Code, response.Body.String())
	}
	return response.Body.Bytes()
}

// settingsOf decodes a settings answer.
func settingsOf(t *testing.T, raw []byte) projectDeliverySettings {
	t.Helper()
	var settings projectDeliverySettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("decode the delivery settings %s: %v", raw, err)
	}
	return settings
}

// refusedSettings asserts response is the settings refusal of code at status, and answers it.
func refusedSettings(t *testing.T, response *httptest.ResponseRecorder, status int, code string) delivery.SettingsError {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d body=%s, want %d %s", response.Code, response.Body.String(), status, code)
	}
	refusal := decodeBody[delivery.SettingsError](t, response)
	if refusal.Code != code || strings.TrimSpace(refusal.Message) == "" {
		t.Fatalf("refusal = %+v, want code %s and a nonempty error", refusal, code)
	}
	return refusal
}

// sourceRepos is each source's resolved repository, "" where it reads none.
func sourceRepos(settings projectDeliverySettings) []string {
	repos := make([]string, len(settings.Sources))
	for i, source := range settings.Sources {
		if source.ResolvedRepo != nil {
			repos[i] = *source.ResolvedRepo
		}
	}
	return repos
}

// Two projects save different delivery settings and each reads back its own: ALPHA two sources in
// two repositories, one naming its repository (stored lower case) and one naming none, which reads
// ALPHA's default repository; BETA one source and delivery off, which reads back off. Each source
// carries its id, and the PUT answers what GET reads. A project with no settings reads null, and
// one whose source names no repository and has no default repository cannot turn delivery on.
func TestEachProjectReadsBackItsOwnDeliverySettings(t *testing.T) {
	rig := newDeliverySettingsRig(t, readableRepos())
	createTestProject(t, rig.handler, "ALPHA", "BETA", "GAMMA")
	setDefaultRepository(t, rig.handler, "ALPHA", "acme/alpha")
	setDefaultRepository(t, rig.handler, "BETA", "acme/beta")

	if raw := readProjectDelivery(t, rig.handler, "ALPHA"); strings.TrimSpace(string(raw)) != "null" {
		t.Fatalf("ALPHA before any save reads %s, want null", raw)
	}
	alpha := savedProjectDelivery(t, rig.handler, "ALPHA", map[string]any{
		"delivery": true,
		"sources": []any{
			workflowSource("Acme/Alpha-Deploy", ".github/workflows/deploy.yml", "release / release"),
			workflowSource("", ".github/workflows/ship.yml", "ship / ship"),
		},
		"population_authors": []string{"octocat"}, "failure_sources": []string{delivery.FailureSourceReverts},
		"failures_measured_since": "2026-11-01",
	})
	beta := savedProjectDelivery(t, rig.handler, "BETA", map[string]any{
		"delivery": false, "sources": []any{workflowSource("", ".github/workflows/beta.yml", "beta / release")},
		"population_authors": []string{"hubot"},
	})

	gotAlpha := settingsOf(t, readProjectDelivery(t, rig.handler, "ALPHA"))
	gotBeta := settingsOf(t, readProjectDelivery(t, rig.handler, "BETA"))
	if !slices.Equal(sourceRepos(gotAlpha), []string{"acme/alpha-deploy", "acme/alpha"}) || !gotAlpha.Delivery ||
		gotAlpha.Sources[0].Repo == nil || *gotAlpha.Sources[0].Repo != "acme/alpha-deploy" || gotAlpha.Sources[1].Repo != nil {
		t.Fatalf("ALPHA reads back %+v, want delivery on, acme/alpha-deploy named and a source reading its default acme/alpha", gotAlpha)
	}
	if !slices.Equal(gotAlpha.PopulationAuthors, []string{"octocat"}) || !slices.Equal(gotAlpha.FailureSources, []string{"reverts"}) ||
		gotAlpha.FailuresMeasuredSince == nil || *gotAlpha.FailuresMeasuredSince != "2026-11-01" {
		t.Fatalf("ALPHA reads back population %v, failure sources %v from %v, want octocat, reverts from 2026-11-01",
			gotAlpha.PopulationAuthors, gotAlpha.FailureSources, gotAlpha.FailuresMeasuredSince)
	}
	if gotBeta.Delivery || !slices.Equal(sourceRepos(gotBeta), []string{"acme/beta"}) || !slices.Equal(gotBeta.PopulationAuthors, []string{"hubot"}) {
		t.Fatalf("BETA reads back %+v, want delivery off, one source reading acme/beta and hubot's population", gotBeta)
	}
	for _, source := range slices.Concat(gotAlpha.Sources, gotBeta.Sources) {
		if source.ID == "" || source.ReconciledThrough != nil {
			t.Fatalf("source %+v carries no id or a reconcile no pass made", source)
		}
	}
	for _, pair := range []struct {
		name      string
		put, read projectDeliverySettings
	}{{"ALPHA", alpha, gotAlpha}, {"BETA", beta, gotBeta}} {
		put, _ := json.Marshal(pair.put)
		read, _ := json.Marshal(pair.read)
		if string(put) != string(read) {
			t.Errorf("%s's save answered %s, and GET reads %s", pair.name, put, read)
		}
	}

	refusal := refusedSettings(t, putProjectDelivery(t, rig.handler, "GAMMA", map[string]any{
		"delivery": true, "sources": []any{workflowSource("", ".github/workflows/deploy.yml", "release")},
	}), http.StatusConflict, delivery.CodeDeliveryNoDefaultRepository)
	if refusal.Source == nil || *refusal.Source != 0 || !strings.Contains(refusal.Message, "default repository") {
		t.Fatalf("GAMMA's refusal %+v names no source or default repository", refusal)
	}
	if raw := readProjectDelivery(t, rig.handler, "GAMMA"); strings.TrimSpace(string(raw)) != "null" {
		t.Fatalf("GAMMA after its refused save reads %s, want null", raw)
	}
	bad := refusedSettings(t, putProjectDelivery(t, rig.handler, "BETA", map[string]any{
		"delivery": true, "sources": []any{workflowSource("", ".github/workflows/beta.yml", "beta / release"), map[string]any{"kind": "github_workflow", "repo": "acme/beta-deploy"}},
	}), http.StatusBadRequest, delivery.CodeDeliverySettingsInput)
	if bad.Source == nil || *bad.Source != 1 || bad.Field != "deploy_workflow_path" {
		t.Fatalf("a source with no workflow is refused %+v, want source 1's deploy_workflow_path named", bad)
	}
}

// evalVersion is the version and target evals_current holds of evalID.
func evalVersion(t *testing.T, rig *brokerTokenRig, evalID string) (int, *float64) {
	t.Helper()
	var version int
	var target *float64
	if err := rig.database.Pool.QueryRow(t.Context(), `select version, target from evals_current where eval_id = $1`, evalID).Scan(&version, &target); err != nil {
		t.Fatalf("read %s: %v", evalID, err)
	}
	return version, target
}

// A number target and an off target saved with the settings are each a new version of the
// project's eval row, read back through evals_current and through GET; a save that names the
// targets the rows hold writes no version; no column of the settings' tables holds a target; and a
// target that is neither a number nor "off", or of an eval the settings do not set, is refused.
func TestTheSettingsTargetsAreNewVersionsOfTheProjectsEvalRows(t *testing.T) {
	rig := newDeliverySettingsRig(t, readableRepos())
	createTestProject(t, rig.handler, "ALPHA")
	body := map[string]any{
		"delivery": true, "sources": []any{workflowSource("acme/alpha-deploy", ".github/workflows/deploy.yml", "release")},
		"population_authors": []string{"octocat"},
		"targets":            map[string]any{"deploy-frequency": 7, "change-fail-rate": "off"},
	}
	frequencyBefore, _ := evalVersion(t, rig, "ALPHA/deploy-frequency")
	failBefore, failTarget := evalVersion(t, rig, "ALPHA/change-fail-rate")
	if failTarget == nil {
		t.Fatal("ALPHA's change-fail-rate holds no target, and the check needs one to turn off")
	}
	saved := savedProjectDelivery(t, rig.handler, "ALPHA", body)
	frequencyAfter, frequencyTarget := evalVersion(t, rig, "ALPHA/deploy-frequency")
	failAfter, failTarget := evalVersion(t, rig, "ALPHA/change-fail-rate")
	if frequencyAfter != frequencyBefore+1 || frequencyTarget == nil || *frequencyTarget != 7 {
		t.Fatalf("deploy-frequency holds version %d target %v, want version %d target 7", frequencyAfter, frequencyTarget, frequencyBefore+1)
	}
	if failAfter != failBefore+1 || failTarget != nil {
		t.Fatalf("change-fail-rate holds version %d target %v, want version %d with no target", failAfter, failTarget, failBefore+1)
	}
	var targets map[string]any
	if err := json.Unmarshal(saved.Targets, &targets); err != nil {
		t.Fatalf("decode the targets %s: %v", saved.Targets, err)
	}
	if targets["deploy-frequency"] != 7.0 || targets["change-fail-rate"] != deliveryTargetOff || len(targets) != len(deliverySettingsTargetSlugs) {
		t.Fatalf("the save answers targets %v, want deploy-frequency 7, change-fail-rate off and every one of the seven", targets)
	}
	if read := settingsOf(t, readProjectDelivery(t, rig.handler, "ALPHA")); string(read.Targets) != string(saved.Targets) {
		t.Fatalf("GET reads targets %s, the save answered %s", read.Targets, saved.Targets)
	}

	savedProjectDelivery(t, rig.handler, "ALPHA", body)
	if again, _ := evalVersion(t, rig, "ALPHA/deploy-frequency"); again != frequencyAfter {
		t.Fatalf("a save of the target the row holds wrote version %d, want it left at %d", again, frequencyAfter)
	}
	var columns int
	if err := rig.database.Pool.QueryRow(t.Context(), `
		select count(*) from information_schema.columns
		where table_name in ('project_delivery_settings', 'project_deploy_sources') and column_name like '%target%'
	`).Scan(&columns); err != nil {
		t.Fatalf("read the settings' columns: %v", err)
	}
	if columns != 0 {
		t.Fatalf("%d columns of the settings' tables name a target, want none", columns)
	}
	for target, field := range map[string]map[string]any{
		"targets.deploy-frequency": {"deploy-frequency": "high"},
		"targets.pr-ci":            {"pr-ci": 3},
	} {
		refused := refusedSettings(t, putProjectDelivery(t, rig.handler, "ALPHA", map[string]any{
			"delivery": true, "sources": body["sources"], "population_authors": body["population_authors"], "targets": field,
		}), http.StatusBadRequest, delivery.CodeDeliverySettingsInput)
		if refused.Field != target {
			t.Errorf("targets %v refused naming %q, want %q", field, refused.Field, target)
		}
	}
}

// A source's reconciled_through is null until its repository's deploy listing, its pull-request
// checks listing and every installation's merged-pull-request search under the current scope each
// hold a row, and then the earliest of them.
func TestReconciledThroughIsTheEarliestStepOfTheSourcesRepository(t *testing.T) {
	rig := newDeliverySettingsRig(t, readableRepos())
	createTestProject(t, rig.handler, "ALPHA")
	savedProjectDelivery(t, rig.handler, "ALPHA", map[string]any{
		"delivery": true, "sources": []any{workflowSource("acme/alpha-deploy", ".github/workflows/deploy.yml", "release")},
		"population_authors": []string{"octocat"},
	})
	through := func() *time.Time {
		return settingsOf(t, readProjectDelivery(t, rig.handler, "ALPHA")).Sources[0].ReconciledThrough
	}
	if got := through(); got != nil {
		t.Fatalf("with no progress the source reads reconciled_through %v, want null", got)
	}
	deployed, checked, searched := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC), time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC), time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	for _, step := range []struct {
		step, scope string
		through     time.Time
	}{
		{"runs/deploy/acme/alpha-deploy", "acme/alpha-deploy .github/workflows/deploy.yml", deployed},
		{"runs/pr_checks/acme/alpha-deploy", "acme/alpha-deploy .github/workflows/ci.yml", checked},
	} {
		if err := delivery.RecordReconcileProgress(t.Context(), rig.database.Pool, step.step, step.scope, step.through); err != nil {
			t.Fatalf("record %s: %v", step.step, err)
		}
		if got := through(); got != nil {
			t.Fatalf("with %s and no search the source reads reconciled_through %v, want null", step.step, got)
		}
	}
	if err := delivery.RecordReconcileProgress(t.Context(), rig.database.Pool, "merged_pull_requests/installation/1",
		"authors=octocat excluded= deploy=acme/alpha-deploy", searched); err != nil {
		t.Fatalf("record the search: %v", err)
	}
	if got := through(); got == nil || !got.Equal(checked) {
		t.Fatalf("with every step the source reads reconciled_through %v, want the earliest, %s", got, checked)
	}
}

// The settings are the deployment's to write: a token the broker scoped to one project is refused
// PROJECT_SCOPE_DEPLOYMENT on that very project, and reads them. A save is refused, writing
// nothing, where the App cannot read a source's repository, naming the source, and where another
// delivery-on project's source reads a repository, naming that project, or delivery is turned on
// without a repository to read. A save that turns delivery off proves no repository, and reads
// back off. A read-only token and the shared agent token read the same settings as a person.
func TestTheProjectDeliverySettingsAreTheDeploymentsWrite(t *testing.T) {
	rig := newDeliverySettingsRig(t, readableRepos())
	caller := newBrokerCaller(t)
	const token = "a-token-that-may-not-write-delivery-settings"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")
	createTestProject(t, rig.handler, "ALPHA", "BARE")
	path := "/api/v1/projects/" + brokerTestProject + "/settings/delivery"
	settings := map[string]any{
		"delivery": true, "sources": []any{workflowSource("acme/beta-deploy", ".github/workflows/deploy.yml", "release")},
		"population_authors": []string{"octocat"},
	}

	refused := rig.serve(caller.presenting(t, http.MethodPut, path, token, settings))
	if refused.Code != http.StatusForbidden || decodeBody[refusalAnswer](t, refused).Code != codeProjectScopeDeployment {
		t.Fatalf("a project-scoped token: status=%d body=%s, want 403 %s", refused.Code, refused.Body.String(), codeProjectScopeDeployment)
	}
	read := rig.serve(caller.presenting(t, http.MethodGet, path, token, nil))
	if read.Code != http.StatusOK || strings.TrimSpace(read.Body.String()) != "null" {
		t.Fatalf("the project-scoped token's read: status=%d body=%s, want 200 null", read.Code, read.Body.String())
	}

	denied := refusedSettings(t, putProjectDelivery(t, rig.handler, brokerTestProject, map[string]any{
		"delivery": true, "population_authors": []string{"octocat"},
		"sources": []any{workflowSource("acme/beta-deploy", ".github/workflows/deploy.yml", "release"), workflowSource("acme/secret", ".github/workflows/deploy.yml", "release")},
	}), http.StatusConflict, delivery.CodeDeliverySettingsAccess)
	if denied.Source == nil || *denied.Source != 1 || !strings.Contains(denied.Message, "acme/secret") {
		t.Fatalf("the access refusal %+v does not name source 1 and acme/secret", denied)
	}
	if raw := readProjectDelivery(t, rig.handler, brokerTestProject); strings.TrimSpace(string(raw)) != "null" {
		t.Fatalf("the refused save left %s, want null", raw)
	}

	savedProjectDelivery(t, rig.handler, "ALPHA", settings)
	setDefaultRepository(t, rig.handler, brokerTestProject, "acme/beta")
	savedProjectDelivery(t, rig.handler, brokerTestProject, map[string]any{
		"delivery": false, "sources": []any{workflowSource("", ".github/workflows/ship.yml", "ship")},
		"population_authors": []string{"hubot"},
	})
	savedProjectDelivery(t, rig.handler, "BARE", map[string]any{
		"delivery": false, "sources": []any{workflowSource("", ".github/workflows/ship.yml", "ship")},
		"population_authors": []string{"hubot"},
	})
	for _, row := range []struct {
		name, project, code, named string
		body                       map[string]any
	}{
		{"another delivery-on project reads the repository", brokerTestProject, delivery.CodeDeliveryRepositoryShared, "ALPHA", settings},
		{"no repository to read", "BARE", delivery.CodeDeliveryNoDefaultRepository, "default repository", map[string]any{"delivery": true}},
	} {
		t.Run(row.name, func(t *testing.T) {
			before := readProjectDelivery(t, rig.handler, row.project)
			owner := readProjectDelivery(t, rig.handler, "ALPHA")
			refusal := refusedSettings(t, putProjectDelivery(t, rig.handler, row.project, row.body), http.StatusConflict, row.code)
			if !strings.Contains(refusal.Message, row.named) || refusal.Source == nil || *refusal.Source != 0 ||
				(row.code == delivery.CodeDeliveryRepositoryShared && refusal.Project != row.named) {
				t.Errorf("refusal %+v does not name source 0 and %s", refusal, row.named)
			}
			if after := readProjectDelivery(t, rig.handler, row.project); string(after) != string(before) {
				t.Errorf("the refused save left %s's settings %s, want %s", row.project, after, before)
			}
			if after := readProjectDelivery(t, rig.handler, "ALPHA"); string(after) != string(owner) {
				t.Errorf("the refused save left ALPHA's settings %s, want %s", after, owner)
			}
		})
	}

	const reader = "a-read-only-token-reading-delivery-settings"
	rig.broker.Says(reader, readOnlyToken(caller.jkt))
	want := readProjectDelivery(t, rig.handler, brokerTestProject)
	for _, row := range []struct {
		name    string
		request *http.Request
	}{
		{"a read-only token", caller.presenting(t, http.MethodGet, path, reader, nil)},
		{"the shared agent token", brokerRequest(t, http.MethodGet, path, nil, "Bearer agent-token", "")},
	} {
		t.Run(row.name, func(t *testing.T) {
			read := rig.serve(row.request)
			if read.Code != http.StatusOK || read.Body.String() != string(want) {
				t.Errorf("%s reads %d %s, want 200 %s", row.name, read.Code, read.Body.String(), want)
			}
		})
	}

	off := savedProjectDelivery(t, rig.handler, brokerTestProject, map[string]any{
		"delivery": false, "sources": []any{workflowSource("acme/secret", ".github/workflows/deploy.yml", "release")},
	})
	if off.Delivery || settingsOf(t, readProjectDelivery(t, rig.handler, brokerTestProject)).Delivery {
		t.Fatalf("a save with delivery off answered %+v, want it off", off)
	}
}

// A null target is neither a number nor "off": the save is refused naming the target, and writes
// neither the settings nor a version of the eval, where read as a number it would set a target of 0.
func TestANullTargetIsRefused(t *testing.T) {
	rig := newDeliverySettingsRig(t, readableRepos())
	createTestProject(t, rig.handler, "ALPHA")
	version, target := evalVersion(t, rig, "ALPHA/deploy-frequency")
	refused := refusedSettings(t, putProjectDelivery(t, rig.handler, "ALPHA", map[string]any{
		"delivery": true, "sources": []any{workflowSource("acme/alpha-deploy", ".github/workflows/deploy.yml", "release")},
		"population_authors": []string{"octocat"}, "targets": map[string]any{"deploy-frequency": nil},
	}), http.StatusBadRequest, delivery.CodeDeliverySettingsInput)
	if refused.Field != "targets.deploy-frequency" {
		t.Errorf("a null target is refused naming %q, want targets.deploy-frequency", refused.Field)
	}
	if after, held := evalVersion(t, rig, "ALPHA/deploy-frequency"); after != version || !equalPointers(held, target) {
		t.Errorf("after the refused save deploy-frequency holds version %d target %v, want version %d target %v", after, held, version, target)
	}
	if raw := readProjectDelivery(t, rig.handler, "ALPHA"); strings.TrimSpace(string(raw)) != "null" {
		t.Errorf("the refused save left settings %s, want none", raw)
	}
}

// A save naming delivery null is refused, naming the field, and leaves the switch as it stood: null
// clears a field that may be empty, and the switch is never empty, so a body whose null the decode
// would read as false must not turn a project's delivery off, which drops its delivery section,
// its delivery evals and its failure measures (found by INFRA-978's tester at a37567ad, where the
// save answered 200 and read back delivery off).
func TestANullDeliverySwitchIsRefused(t *testing.T) {
	t.Parallel()
	rig := newDeliverySettingsRig(t, readableRepos())
	createTestProject(t, rig.handler, "ALPHA")
	savedProjectDelivery(t, rig.handler, "ALPHA", map[string]any{
		"delivery": true, "sources": []any{workflowSource("acme/alpha-deploy", ".github/workflows/deploy.yml", "release")},
		"population_authors": []string{"octocat"},
	})
	before := readProjectDelivery(t, rig.handler, "ALPHA")
	refused := refusedSettings(t, putProjectDelivery(t, rig.handler, "ALPHA", map[string]any{"delivery": nil}),
		http.StatusBadRequest, delivery.CodeDeliverySettingsInput)
	if refused.Field != "delivery" {
		t.Errorf("a null switch is refused naming %q, want delivery", refused.Field)
	}
	if after := readProjectDelivery(t, rig.handler, "ALPHA"); string(after) != string(before) {
		t.Errorf("the refused save left settings %s, want %s", after, before)
	}
}

// newSavedDeliveryRig is a settings rig whose project ALPHA has saved delivery on, one source
// reading acme/alpha-deploy, octocat's population and acme/dojo excluded.
func newSavedDeliveryRig(t *testing.T) *brokerTokenRig {
	t.Helper()
	rig := newDeliverySettingsRig(t, readableRepos())
	createTestProject(t, rig.handler, "ALPHA")
	savedProjectDelivery(t, rig.handler, "ALPHA", map[string]any{
		"delivery": true, "sources": []any{workflowSource("acme/alpha-deploy", ".github/workflows/deploy.yml", "release")},
		"population_authors": []string{"octocat"}, "excluded_repos": []string{"acme/dojo"},
	})
	return rig
}

// requireInputRefused sends body as project's delivery settings and requires the refusal 400
// DELIVERY_SETTINGS_INPUT naming field and no source, with nothing written: the settings GET
// reads, and the version and target of each eval the settings set, as they stood before.
func requireInputRefused(t *testing.T, rig *brokerTokenRig, project string, body map[string]any, field string) {
	t.Helper()
	before := readProjectDelivery(t, rig.handler, project)
	versions := make(map[string]int, len(deliverySettingsTargetSlugs))
	targets := make(map[string]*float64, len(deliverySettingsTargetSlugs))
	for _, slug := range deliverySettingsTargetSlugs {
		versions[slug], targets[slug] = evalVersion(t, rig, project+"/"+slug)
	}
	refused := refusedSettings(t, putProjectDelivery(t, rig.handler, project, body), http.StatusBadRequest, delivery.CodeDeliverySettingsInput)
	if refused.Field != field || refused.Source != nil {
		t.Errorf("%v is refused %+v, want %s named and no source", body, refused, field)
	}
	if after := readProjectDelivery(t, rig.handler, project); string(after) != string(before) {
		t.Errorf("the refused save of %v left settings %s, want %s", body, after, before)
	}
	for _, slug := range deliverySettingsTargetSlugs {
		if version, target := evalVersion(t, rig, project+"/"+slug); version != versions[slug] || !equalPointers(target, targets[slug]) {
			t.Errorf("the refused save of %v left %s at version %d target %v, want version %d target %v",
				body, slug, version, target, versions[slug], targets[slug])
		}
	}
}

// A negative target is refused naming the target, whatever its eval's unit (deploys a day, minutes
// or a share), and writes nothing: neither a version of any eval nor the switch turned off and the
// population the body also names.
func TestANegativeTargetIsRefused(t *testing.T) {
	t.Parallel()
	rig := newSavedDeliveryRig(t)
	for _, row := range []struct {
		slug   string
		target float64
	}{{"deploy-frequency", -3}, {"merge-to-production", -1}, {"deploy-run-success", -0.1}} {
		t.Run(row.slug, func(t *testing.T) {
			requireInputRefused(t, rig, "ALPHA", map[string]any{
				"delivery": false, "population_authors": []string{"hubot"}, "targets": map[string]any{row.slug: row.target},
			}, "targets."+row.slug)
		})
	}
}

// A target of an eval whose unit is a share (ratio, from the eval the catalogue seeds) lies from 0
// to 1: 7 and 90 for deploy-run-success, and 1.5 for change-fail-rate, are each refused naming the
// target and write nothing, the population the body also names included. 0 and 1 are each saved as
// a new version of deploy-run-success, and 90, refused there, is a new version of
// merge-to-production, an eval of minutes.
func TestARatioTargetOutside0To1IsRefused(t *testing.T) {
	t.Parallel()
	rig := newSavedDeliveryRig(t)
	for _, row := range []struct {
		name, slug string
		target     float64
	}{
		{"deploy-run-success 7", "deploy-run-success", 7},
		{"deploy-run-success 90", "deploy-run-success", 90},
		{"change-fail-rate 1.5", "change-fail-rate", 1.5},
	} {
		t.Run(row.name, func(t *testing.T) {
			requireInputRefused(t, rig, "ALPHA", map[string]any{
				"population_authors": []string{"hubot"}, "targets": map[string]any{row.slug: row.target},
			}, "targets."+row.slug)
		})
	}
	for _, row := range []struct {
		slug   string
		target float64
	}{{"deploy-run-success", 0}, {"deploy-run-success", 1}, {"merge-to-production", 90}} {
		evalID := "ALPHA/" + row.slug
		version, _ := evalVersion(t, rig, evalID)
		savedProjectDelivery(t, rig.handler, "ALPHA", map[string]any{"targets": map[string]any{row.slug: row.target}})
		if after, held := evalVersion(t, rig, evalID); after != version+1 || held == nil || *held != row.target {
			t.Errorf("a save of %s's target %v left version %d target %v, want version %d target %v",
				row.slug, row.target, after, held, version+1, row.target)
		}
	}
}

// A population author that is empty, or white space alone, is refused naming population_authors,
// and writes nothing, the switch the body turns off included.
func TestABlankPopulationAuthorIsRefused(t *testing.T) {
	t.Parallel()
	rig := newSavedDeliveryRig(t)
	for _, row := range []struct {
		name    string
		authors []string
	}{
		{"an empty author", []string{""}},
		{"a blank author after a login", []string{"hubot", "  "}},
	} {
		t.Run(row.name, func(t *testing.T) {
			requireInputRefused(t, rig, "ALPHA", map[string]any{"delivery": false, "population_authors": row.authors}, "population_authors")
		})
	}
}

// An excluded repository that is no GitHub owner/name, as a source's repository is read
// (parseSourceRepo), is refused naming excluded_repos and writes nothing, the switch the body turns
// off included; one that is, in any case, with space around it and a trailing .git, is stored as a
// source's repository is: trimmed and lower-cased.
func TestAnExcludedRepositoryThatIsNoOwnerNameIsRefused(t *testing.T) {
	t.Parallel()
	rig := newSavedDeliveryRig(t)
	for _, row := range []struct {
		name     string
		excluded []string
	}{
		{"no owner", []string{"dojo"}},
		{"three parts", []string{"acme/dojo/extra"}},
		{"an empty entry after a repository", []string{"acme/other", ""}},
		{"a space inside", []string{"acme/do jo"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			requireInputRefused(t, rig, "ALPHA", map[string]any{"delivery": false, "excluded_repos": row.excluded}, "excluded_repos")
		})
	}
	saved := savedProjectDelivery(t, rig.handler, "ALPHA", map[string]any{"excluded_repos": []string{" Acme/Other.git "}})
	if !slices.Equal(saved.ExcludedRepos, []string{"acme/other"}) || !saved.Delivery || !slices.Equal(saved.PopulationAuthors, []string{"octocat"}) {
		t.Errorf("a save of the excluded repository \" Acme/Other.git \" holds %+v, want acme/other excluded, and delivery on and octocat kept", saved)
	}
}

// A save's job names are stored as the singleton's save stores them, since every reader matches a
// job by its exact name: each production job name trimmed, and each optional job and step name
// trimmed, a blank one stored as null. A production job name that is blank or named twice once
// trimmed, and a workflow path of white space alone, are refused, naming the field and the source.
func TestASavesJobNamesAreStoredAsTheSingletonsSaveStoresThem(t *testing.T) {
	rig := newDeliverySettingsRig(t, readableRepos())
	createTestProject(t, rig.handler, "ALPHA")
	source := workflowSource("acme/alpha-deploy", ".github/workflows/deploy.yml", "release")
	source["production_job_names"] = []string{" release / release ", "deploy\t"}
	source["production_apply_step_name"] = "  Apply  "
	source["staging_e2e_job_name"] = ""
	source["staging_apply_job_name"] = "   "
	source["pr_checks_rollup_job_name"] = " checks-complete"
	body := func(sources ...any) map[string]any {
		return map[string]any{"delivery": true, "sources": sources, "population_authors": []string{"octocat"}}
	}
	saved := savedProjectDelivery(t, rig.handler, "ALPHA", body(source)).Sources[0]
	if !slices.Equal(saved.ProductionJobNames, []string{"release / release", "deploy"}) || deref(saved.ProductionApplyStepName) != "Apply" ||
		saved.StagingE2EJobName != nil || saved.StagingApplyJobName != nil || deref(saved.PRChecksRollupJobName) != "checks-complete" {
		t.Fatalf("the source is stored with production jobs %q, apply step %v, staging jobs %v and %v, roll-up %v; want each trimmed and the blank ones null",
			saved.ProductionJobNames, saved.ProductionApplyStepName, saved.StagingE2EJobName, saved.StagingApplyJobName, saved.PRChecksRollupJobName)
	}
	for _, bad := range []struct {
		what, field, key string
		value            any
	}{
		{"a production job named twice once trimmed", "production_job_names", "production_job_names", []string{"ship", " ship"}},
		{"a blank production job", "production_job_names", "production_job_names", []string{"ship", "  "}},
		{"a deploy workflow of white space", "deploy_workflow_path", "deploy_workflow_path", "  "},
		{"a pull-request checks workflow of white space", "pr_checks_workflow_path", "pr_checks_workflow_path", "\t"},
	} {
		second := workflowSource("acme/alpha", ".github/workflows/ship.yml", "ship")
		second[bad.key] = bad.value
		refusal := refusedSettings(t, putProjectDelivery(t, rig.handler, "ALPHA", body(source, second)), http.StatusBadRequest, delivery.CodeDeliverySettingsInput)
		if refusal.Field != bad.field || refusal.Source == nil || *refusal.Source != 1 {
			t.Errorf("%s is refused %+v, want source 1's %s named", bad.what, refusal, bad.field)
		}
	}
}

// A save keeps the stored value of each top-level field its body leaves out (the two-build rule,
// store/README.md: a page of a build that does not know a field saves without it): a PUT without
// failure_sources and without population_authors keeps both, with the day the failures count from
// and the excluded repository it names; one naming population_authors null clears it and keeps
// the switch and the source, its row and id included; one naming sources replaces the list; and
// naming one target changes that target alone.
func TestAPutThatLeavesOutAFieldKeepsItsStoredValue(t *testing.T) {
	t.Parallel()
	rig := newDeliverySettingsRig(t, readableRepos())
	createTestProject(t, rig.handler, "ALPHA")
	source := workflowSource("acme/alpha-deploy", ".github/workflows/deploy.yml", "release")
	first := savedProjectDelivery(t, rig.handler, "ALPHA", map[string]any{
		"delivery": true, "sources": []any{source}, "population_authors": []string{"octocat"},
		"excluded_repos": []string{"acme/dojo"}, "failure_sources": []string{delivery.FailureSourceReverts},
		"failures_measured_since": "2026-11-01",
	})

	kept := savedProjectDelivery(t, rig.handler, "ALPHA", map[string]any{
		"delivery": true, "sources": []any{source}, "excluded_repos": []string{"acme/other"},
	})
	if !slices.Equal(kept.PopulationAuthors, []string{"octocat"}) || !slices.Equal(kept.FailureSources, []string{"reverts"}) ||
		deref(kept.FailuresMeasuredSince) != "2026-11-01" || !slices.Equal(kept.ExcludedRepos, []string{"acme/other"}) {
		t.Fatalf("a save without population_authors and failure_sources holds population %v, failure sources %v from %v and excluded %v; want octocat, reverts from 2026-11-01 kept and acme/other saved",
			kept.PopulationAuthors, kept.FailureSources, kept.FailuresMeasuredSince, kept.ExcludedRepos)
	}

	cleared := savedProjectDelivery(t, rig.handler, "ALPHA", map[string]any{"population_authors": nil})
	if len(cleared.PopulationAuthors) != 0 || !cleared.Delivery || len(cleared.Sources) != 1 || cleared.Sources[0].ID != first.Sources[0].ID ||
		!slices.Equal(cleared.FailureSources, []string{"reverts"}) || !slices.Equal(cleared.ExcludedRepos, []string{"acme/other"}) {
		t.Fatalf("a save of population_authors null holds %+v; want no population author, and delivery on, source %s, reverts and acme/other kept",
			cleared, first.Sources[0].ID)
	}

	replaced := savedProjectDelivery(t, rig.handler, "ALPHA", map[string]any{
		"sources": []any{workflowSource("acme/alpha", ".github/workflows/ship.yml", "ship")},
	})
	if !slices.Equal(sourceRepos(replaced), []string{"acme/alpha"}) || !replaced.Delivery {
		t.Fatalf("a save naming a replacement source holds %v and delivery %v; want acme/alpha alone and delivery on",
			sourceRepos(replaced), replaced.Delivery)
	}

	failVersion, failTarget := evalVersion(t, rig, "ALPHA/change-fail-rate")
	targeted := savedProjectDelivery(t, rig.handler, "ALPHA", map[string]any{"targets": map[string]any{"deploy-frequency": 9}})
	if version, target := evalVersion(t, rig, "ALPHA/deploy-frequency"); target == nil || *target != 9 {
		t.Fatalf("a save of one target leaves deploy-frequency at version %d target %v, want 9", version, target)
	}
	if version, target := evalVersion(t, rig, "ALPHA/change-fail-rate"); version != failVersion || !equalPointers(target, failTarget) {
		t.Fatalf("a save of deploy-frequency's target moved change-fail-rate to version %d target %v, want version %d target %v", version, target, failVersion, failTarget)
	}
	if !slices.Equal(sourceRepos(targeted), sourceRepos(replaced)) || !targeted.Delivery || len(targeted.PopulationAuthors) != 0 {
		t.Fatalf("a save of one target holds %+v, want the settings as the save before left them", targeted)
	}

	empty := savedProjectDelivery(t, rig.handler, "ALPHA", map[string]any{
		"delivery": false, "sources": nil, "excluded_repos": nil, "failure_sources": nil, "failures_measured_since": nil,
	})
	if empty.Delivery || len(empty.Sources) != 0 || len(empty.ExcludedRepos) != 0 || len(empty.FailureSources) != 0 || empty.FailuresMeasuredSince != nil {
		t.Errorf("a save turning delivery off and clearing the optional fields left %+v, want those fields empty", empty)
	}
}

// A workflow and an events source save and read back together; the events source names no
// repository or workflow. A second events source is refused by name and leaves the record alone.
func TestAnEventsSourceIsSavedAndReadBackThroughTheRoute(t *testing.T) {
	t.Parallel()
	rig := newDeliverySettingsRig(t, readableRepos())
	createTestProject(t, rig.handler, "ALPHA")
	sources := []any{workflowSource("acme/alpha", ".github/workflows/deploy.yml", "release"), map[string]any{"kind": "events"}}
	body := map[string]any{"delivery": true, "sources": sources}
	saved := savedProjectDelivery(t, rig.handler, "ALPHA", body)
	before := readProjectDelivery(t, rig.handler, "ALPHA")
	read := settingsOf(t, before)
	if want := mustJSON(t, saved); mustJSON(t, read) != want {
		t.Fatalf("GET reads %s, want the settings PUT answered: %s", before, want)
	}
	if !read.Delivery || len(read.Sources) != 2 || read.Sources[0].Kind != delivery.SourceKindGitHubWorkflow ||
		deref(read.Sources[0].Repo) != "acme/alpha" || deref(read.Sources[0].DeployWorkflowPath) != ".github/workflows/deploy.yml" {
		t.Fatalf("GET reads %s, want delivery on with the workflow and events sources", before)
	}
	event := read.Sources[1]
	if event.Kind != delivery.SourceKindEvents || event.ID == "" || event.Repo != nil || event.ResolvedRepo != nil ||
		event.DeployWorkflowPath != nil || event.PRChecksWorkflowPath != nil || event.ProductionJobNames != nil {
		t.Errorf("events source = %+v, want an events source with an id and no repository or workflow", event)
	}
	body["sources"] = append(sources, map[string]any{"kind": "events"})
	refusal := refusedSettings(t, putProjectDelivery(t, rig.handler, "ALPHA", body), http.StatusBadRequest, delivery.CodeDeliverySettingsInput)
	if refusal.Source == nil || *refusal.Source != 2 || refusal.Field != "kind" || !strings.Contains(refusal.Message, "one events source at most") {
		t.Errorf("refusal %+v does not name the second events source (index 2)", refusal)
	}
	if after := readProjectDelivery(t, rig.handler, "ALPHA"); string(after) != string(before) {
		t.Errorf("the refused save left %s, want %s", after, before)
	}
}

// projectLockWait matches the statement at which a save waits for its project's delivery lock
// (delivery.LockProject), as waitForLockWait reads it.
const projectLockWait = `%pg_advisory_xact_lock(6902, hashtext(%`

// heldSaveLocks counts the locks of a settings save that a session of the current database other
// than $1 holds: a project's delivery lock (6902) and a repository's (6903), each of two keys
// (objsubid 2: delivery.LockProject, LockRepositories), and the lock of each eval the database
// holds, of one key whose high half is classid (objsubid 1: evals' lockEvals).
const heldSaveLocks = `
	select count(*) from pg_locks
	where locktype = 'advisory' and granted and pid <> $1
		and database = (select oid from pg_database where datname = current_database())
		and ((objsubid = 2 and classid in (6902, 6903))
			or (objsubid = 1 and ((classid::bigint << 32) | objid::bigint) in (
				select hashtext('eval:' || eval_id)::bigint from evals_current)))`

// settingsLockWatch is the settings tests' GitHub seen from the database: each request that
// reaches GitHub (record, githubtest.Spec.Record) fails the test where a session of database other
// than the test's own holder (hold) holds a lock of a settings save (heldSaveLocks), since a save
// asks GitHub nothing while it holds one. It notes the repository of each installation lookup,
// owner/repo, in asked, and hands it to lookup, where set, before GitHub answers.
type settingsLockWatch struct {
	t        *testing.T
	database *store.Store
	lookup   func(repo string)
	// holder is the backend of the test's own transaction holding a project's lock, 0 for none.
	holder atomic.Int32
	mu     sync.Mutex
	asked  []string
}

func (w *settingsLockWatch) record(r *http.Request) {
	if w.database == nil {
		return
	}
	var held int
	if err := w.database.Pool.QueryRow(r.Context(), heldSaveLocks, w.holder.Load()).Scan(&held); err != nil {
		w.t.Errorf("read the locks held while GitHub is asked %s %s: %v", r.Method, r.URL.Path, err)
	} else if held != 0 {
		w.t.Errorf("GitHub is asked %s %s while a session holds %d of a settings save's locks", r.Method, r.URL.Path, held)
	}
	repo, lookup := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/installation")
	if !lookup {
		return
	}
	w.mu.Lock()
	w.asked = append(w.asked, repo)
	w.mu.Unlock()
	if w.lookup != nil {
		w.lookup(repo)
	}
}

// askedSince is each repository GitHub was asked about after the first from of them (asked).
func (w *settingsLockWatch) askedSince(from int) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.asked[from:])
}

// hold begins a transaction that takes project's delivery lock (delivery.LockProject), as a save of
// the project holds it, and leaves its backend out of the watch.
func (w *settingsLockWatch) hold(t *testing.T, project string) pgx.Tx {
	t.Helper()
	holder, err := w.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the holder of %s's lock: %v", project, err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	var pid int32
	if err := holder.QueryRow(t.Context(), `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the holder's backend: %v", err)
	}
	w.holder.Store(pid)
	if err := delivery.LockProject(t.Context(), holder, project); err != nil {
		t.Fatalf("hold %s's lock: %v", project, err)
	}
	return holder
}

// release commits holder, its backend back in the watch first: once committed, its connection
// goes back to the pool, where the save under test may take it.
func (w *settingsLockWatch) release(t *testing.T, holder pgx.Tx) {
	t.Helper()
	w.holder.Store(0)
	if err := holder.Commit(t.Context()); err != nil {
		t.Fatalf("commit the holder: %v", err)
	}
}

// workflowSourceInput is a github_workflow source naming repo as delivery.PutProjectSettings takes
// it, for a write the test makes beside the route's.
func workflowSourceInput(repo string) delivery.SourceInput {
	return delivery.SourceInput{
		Kind: delivery.SourceKindGitHubWorkflow, Repo: new(repo), DeployWorkflowPath: new(".github/workflows/ship.yml"),
		ProductionJobNames: []string{"ship"}, PRChecksWorkflowPath: new(".github/workflows/ci.yml"),
	}
}

// A save keeps the value a field it leaves out holds when the save is written, not when the save
// read it: a PUT naming population_authors alone, sent while another save of the project holds the
// project's lock (delivery.LockProject) with its write made but not committed, waits for that lock
// and then keeps every field the other save wrote, its source's row and id included, beside its own
// population. The other save's source reads a repository the PUT had not proved, so the PUT proves
// it once the lock is free, before its own transaction, and asks GitHub nothing while a session
// but the test's holder holds a save's lock (settingsLockWatch).
func TestAPartialPutThatWaitedForAnotherSaveKeepsWhatThatSaveWrote(t *testing.T) {
	t.Parallel()
	watch := &settingsLockWatch{t: t}
	rig := newDeliverySettingsRigRecording(t, readableRepos(), watch.record)
	watch.database = rig.database
	createTestProject(t, rig.handler, "ALPHA")
	savedProjectDelivery(t, rig.handler, "ALPHA", map[string]any{
		"delivery": true, "sources": []any{workflowSource("acme/alpha-deploy", ".github/workflows/deploy.yml", "release")},
		"population_authors": []string{"octocat"}, "excluded_repos": []string{"acme/dojo"},
	})

	holder := watch.hold(t, "ALPHA")
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	other, err := delivery.PutProjectSettings(t.Context(), holder, delivery.ProjectSettingsInput{
		Project: "ALPHA", Delivery: true, Sources: []delivery.SourceInput{workflowSourceInput("acme/alpha")},
		PopulationAuthors: []string{"octocat"}, ExcludedRepos: []string{"acme/other"},
		FailureSources: []string{delivery.FailureSourceReverts}, FailuresMeasuredSince: &since,
	}, model.Actor{Kind: "user", ID: "bob"})
	if err != nil {
		t.Fatalf("the other save: %v", err)
	}
	from := len(watch.askedSince(0))
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- putProjectDelivery(t, rig.handler, "ALPHA", map[string]any{"population_authors": []string{"hubot"}})
	}()
	waitForLockWait(t, rig.database, projectLockWait)
	watch.release(t, holder)

	response := awaitResponse(t, answered)
	if response.Code != http.StatusOK {
		t.Fatalf("the PUT that waited: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	saved := decodeBody[projectDeliverySettings](t, response)
	if !slices.Equal(saved.PopulationAuthors, []string{"hubot"}) || !saved.Delivery || len(saved.Sources) != 1 ||
		saved.Sources[0].ID != other.Sources[0].ID || !slices.Equal(sourceRepos(saved), []string{"acme/alpha"}) ||
		!slices.Equal(saved.ExcludedRepos, []string{"acme/other"}) ||
		!slices.Equal(saved.FailureSources, []string{delivery.FailureSourceReverts}) || deref(saved.FailuresMeasuredSince) != "2026-11-01" {
		t.Fatalf("the PUT of population_authors alone that waited holds %+v; want hubot, and delivery on, source %s reading acme/alpha, acme/other excluded and reverts from 2026-11-01 as the other save wrote them",
			saved, other.Sources[0].ID)
	}
	if asked := watch.askedSince(from); !slices.Equal(asked, []string{"acme/alpha-deploy", "acme/alpha"}) {
		t.Errorf("the PUT asked GitHub about %q, want acme/alpha-deploy, the source it read first, then acme/alpha, the source the other save wrote", asked)
	}
}

// An enable that waits for another write of its project proves what that write left it reading: a
// PUT turning delivery on, whose source reads the project's default repository, is sent while the
// test holds the project's lock (delivery.LockProject) in a transaction that has made the other
// write but not committed it: a save with delivery off naming another repository, or a move of
// the default repository, which the default-repository route makes under that lock. Once the lock
// is free the PUT finds its source reading a repository it never proved and proves it with no lock
// held (settingsLockWatch): where the GitHub App can read it, the PUT saves, its target with it;
// where it cannot, it refuses 409 DELIVERY_SETTINGS_ACCESS naming the source and writes nothing,
// the settings left as the other write left them and the target with no new version.
func TestAnEnableThatWaitedForAWriteOfItsRepositoryProvesTheRepositoryThatWriteLeft(t *testing.T) {
	t.Parallel()
	fake := readableRepos()
	for _, repo := range []string{"acme/gamma", "acme/delta", "acme/delta-moved"} {
		fake.installations[repo] = "read"
	}
	watch := &settingsLockWatch{t: t}
	rig := newDeliverySettingsRigRecording(t, fake, watch.record)
	watch.database = rig.database
	saveWithDeliveryOff := func(t *testing.T, holder pgx.Tx, project, repo string) {
		t.Helper()
		if _, err := delivery.PutProjectSettings(t.Context(), holder, delivery.ProjectSettingsInput{
			Project: project, Sources: []delivery.SourceInput{workflowSourceInput(repo)},
		}, model.Actor{Kind: "user", ID: "bob"}); err != nil {
			t.Fatalf("save %s's settings with delivery off and a source reading %s: %v", project, repo, err)
		}
	}
	moveDefault := func(t *testing.T, holder pgx.Tx, project, repo string) {
		t.Helper()
		if _, err := holder.Exec(t.Context(), `update projects set default_repo = $2 where key = $1`, project, repo); err != nil {
			t.Fatalf("move %s's default repository to %s: %v", project, repo, err)
		}
	}
	for _, row := range []struct {
		name, project, repo string
		readable            bool
		write               func(t *testing.T, holder pgx.Tx, project, repo string)
	}{
		{"a save with delivery off names a repository the App cannot read", "ALPHA", "acme/secret", false, saveWithDeliveryOff},
		{"a save with delivery off names a repository the App can read", "BETA", "acme/beta-deploy", true, saveWithDeliveryOff},
		{"the default repository moves to one the App cannot read", "GAMMA", "acme/secret", false, moveDefault},
		{"the default repository moves to one the App can read", "DELTA", "acme/delta-moved", true, moveDefault},
	} {
		t.Run(row.name, func(t *testing.T) {
			first := "acme/" + strings.ToLower(row.project)
			createTestProject(t, rig.handler, row.project)
			setDefaultRepository(t, rig.handler, row.project, first)
			savedProjectDelivery(t, rig.handler, row.project, map[string]any{
				"delivery": false, "sources": []any{workflowSource("", ".github/workflows/deploy.yml", "release")},
			})
			evalID := row.project + "/deploy-frequency"
			version, _ := evalVersion(t, rig, evalID)

			holder := watch.hold(t, row.project)
			row.write(t, holder, row.project, row.repo)
			from := len(watch.askedSince(0))
			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				answered <- putProjectDelivery(t, rig.handler, row.project, map[string]any{
					"delivery": true, "targets": map[string]any{"deploy-frequency": 9},
				})
			}()
			waitForLockWait(t, rig.database, projectLockWait)
			watch.release(t, holder)
			response := awaitResponse(t, answered)

			if asked := watch.askedSince(from); !slices.Equal(asked, []string{first, row.repo}) {
				t.Errorf("the enable asked GitHub about %q, want %s, which it read first, then %s, which the other write left", asked, first, row.repo)
			}
			after, target := evalVersion(t, rig, evalID)
			settings := settingsOf(t, readProjectDelivery(t, rig.handler, row.project))
			if !row.readable {
				refusal := refusedSettings(t, response, http.StatusConflict, delivery.CodeDeliverySettingsAccess)
				if refusal.Source == nil || *refusal.Source != 0 || !strings.Contains(refusal.Message, row.repo) {
					t.Errorf("the refusal %+v does not name source 0 and %s", refusal, row.repo)
				}
				if settings.Delivery || !slices.Equal(sourceRepos(settings), []string{row.repo}) || after != version {
					t.Errorf("the refused enable left delivery %v, sources reading %q and %s at version %d; want delivery off, the source reading %s and version %d, as the other write left them",
						settings.Delivery, sourceRepos(settings), evalID, after, row.repo, version)
				}
				return
			}
			if response.Code != http.StatusOK {
				t.Fatalf("the enable: status=%d body=%s, want 200", response.Code, response.Body.String())
			}
			if !settings.Delivery || !slices.Equal(sourceRepos(settings), []string{row.repo}) || after != version+1 || target == nil || *target != 9 {
				t.Errorf("the enable left delivery %v, sources reading %q and %s at version %d target %v; want delivery on, the source reading %s and version %d target 9",
					settings.Delivery, sourceRepos(settings), evalID, after, target, row.repo, version+1)
			}
		})
	}
}

// A save under each of whose attempts (resolutionAttempts) another write moves what it rests on is
// refused 409 DELIVERY_SETTINGS_CHANGED and writes nothing: an enable whose source reads the
// project's default repository, where the default-repository route moves the default while each
// attempt asks GitHub about the one it read (settingsLockWatch.lookup), so each attempt's
// transaction finds the source reading a repository no attempt proved. Each attempt asks about the
// repository no attempt before it proved alone, with no lock held; the settings stay as the moves
// left them, and the target the save names has no new version.
func TestASaveWhoseRepositoryMovesAtEachAttemptIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()
	watch := &settingsLockWatch{t: t}
	rig := newDeliverySettingsRigRecording(t, readableRepos(), watch.record)
	watch.database = rig.database
	createTestProject(t, rig.handler, "ALPHA")
	setDefaultRepository(t, rig.handler, "ALPHA", "acme/alpha")
	savedProjectDelivery(t, rig.handler, "ALPHA", map[string]any{
		"delivery": false, "sources": []any{workflowSource("", ".github/workflows/deploy.yml", "release")},
	})
	before := settingsOf(t, readProjectDelivery(t, rig.handler, "ALPHA"))
	version, target := evalVersion(t, rig, "ALPHA/deploy-frequency")
	moves := map[string]string{"acme/alpha": "acme/beta", "acme/beta": "acme/beta-deploy"}
	watch.lookup = func(repo string) {
		next, moved := moves[repo]
		if !moved {
			return
		}
		response := dispatchRequest(t, rig.handler, http.MethodPut, "/api/v1/projects/ALPHA/default-repository", map[string]any{"repo": next}, "alice")
		if response.Code != http.StatusOK {
			t.Errorf("move ALPHA's default repository to %s while the save asks GitHub about %s: status=%d body=%s", next, repo, response.Code, response.Body.String())
		}
	}

	from := len(watch.askedSince(0))
	refusedSettings(t, putProjectDelivery(t, rig.handler, "ALPHA", map[string]any{
		"delivery": true, "targets": map[string]any{"deploy-frequency": 9},
	}), http.StatusConflict, codeDeliverySettingsChanged)
	if asked := watch.askedSince(from); !slices.Equal(asked, []string{"acme/alpha", "acme/beta"}) {
		t.Errorf("the save asked GitHub about %q, want acme/alpha, then acme/beta, one repository at each of its %d attempts", asked, resolutionAttempts)
	}
	after := settingsOf(t, readProjectDelivery(t, rig.handler, "ALPHA"))
	if after.Delivery || len(after.Sources) != 1 || after.Sources[0].ID != before.Sources[0].ID ||
		!slices.Equal(sourceRepos(after), []string{"acme/beta-deploy"}) || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("the refused save left %+v; want delivery off and source %s, written at %s, reading acme/beta-deploy, where the moves left the default",
			after, before.Sources[0].ID, before.UpdatedAt)
	}
	if held, heldTarget := evalVersion(t, rig, "ALPHA/deploy-frequency"); held != version || !equalPointers(heldTarget, target) {
		t.Errorf("the refused save left deploy-frequency at version %d target %v, want version %d target %v", held, heldTarget, version, target)
	}
}
