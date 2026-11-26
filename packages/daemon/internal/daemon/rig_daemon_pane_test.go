package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// rigToken is the claim token of role on issue in the rig's project ACME.
func rigToken(t *testing.T, issue string, role claim.Role) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken("ACME")
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, issue, role)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// rigRuntime gives the moment's claim to the second runtime only once its real launch is due
// (switchMoment), and every other launch to the first. Probe and the sweep answer each claim from
// the runtime that launched it.
func TestRigRuntimeRoutesEachLaunch(t *testing.T) {
	t.Parallel()
	first, second := fake.NewRuntime(), fake.NewRuntime()
	moment, other := rigToken(t, "ACME-1", claim.RoleImplementer), rigToken(t, "ACME-1", claim.RoleReviewer)
	rt := newRigRuntime(first, second, moment)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spawn := func(token claim.Token) runtime.Locator {
		t.Helper()
		loc, err := rt.Spawn(ctx, runtime.SpawnSpec{Claim: token})
		if err != nil {
			t.Fatalf("spawn %s: %v", token, err)
		}
		return loc
	}
	launches := func(of *fake.Runtime) []string {
		var named []string
		for _, call := range of.Calls() {
			if call.Method == "Spawn" || call.Method == "Resume" {
				named = append(named, call.Method+" "+string(call.Spec.Claim))
			}
		}
		return named
	}

	spawn(moment)
	spawn(other)
	rt.switchMoment()
	held := spawn(moment)
	elsewhere := spawn(other)

	if got, want := launches(first), []string{"Spawn " + string(moment), "Spawn " + string(other), "Spawn " + string(other)}; !slices.Equal(got, want) {
		t.Errorf("the first runtime launched %v, want %v", got, want)
	}
	if got, want := launches(second), []string{"Spawn " + string(moment)}; !slices.Equal(got, want) {
		t.Errorf("the second runtime launched %v, want %v", got, want)
	}

	probed := func(of *fake.Runtime) []runtime.Locator {
		var locators []runtime.Locator
		for _, call := range of.CallsOf("Probe") {
			locators = append(locators, call.Locator)
		}
		return locators
	}
	for _, loc := range []runtime.Locator{held, elsewhere} {
		if _, err := rt.Probe(ctx, loc); err != nil {
			t.Fatalf("probe %s: %v", loc.Claim, err)
		}
	}
	if got := probed(second); len(got) != 1 || got[0] != held {
		t.Errorf("the second runtime was probed at %v, want the moment's launch %v alone", got, held)
	}
	if got := probed(first); len(got) != 1 || got[0] != elsewhere {
		t.Errorf("the first runtime was probed at %v, want the other claim's launch %v alone", got, elsewhere)
	}

	swept, err := rt.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The first runtime's word on the moment's claim, which the second holds, is not passed on;
	// what each runtime says of the claim it holds is. Within one runtime the sweep keeps its order,
	// so once the first runtime's word on the other claim arrives, its word on the moment's claim
	// was read before it.
	first.Emit(runtime.Observation{Locator: held, Kind: runtime.Gone, Detail: "the first runtime"},
		runtime.Observation{Locator: elsewhere, Kind: runtime.Alive, Detail: "the first runtime"})
	second.Emit(runtime.Observation{Locator: held, Kind: runtime.Alive, Detail: "the second runtime"})
	var passed []string
	for len(passed) < 2 {
		select {
		case o := <-swept:
			passed = append(passed, string(o.Locator.Claim)+" from "+o.Detail)
		case <-time.After(10 * time.Second):
			t.Fatalf("the sweep passed on %v within 10s, want two observations", passed)
		}
	}
	slices.Sort(passed)
	want := []string{string(moment) + " from the second runtime", string(other) + " from the first runtime"}
	slices.Sort(want)
	if !slices.Equal(passed, want) {
		t.Errorf("the sweep passed on %v, want %v", passed, want)
	}
}

// The proxy of the intercept control answers report_defect itself and never passes it on; every
// other request reaches the recording handler, which keeps it with the daemon's status.
func TestTheInterceptingProxyAnswersOnlyTheReport(t *testing.T) {
	t.Parallel()
	var reached []string
	var mu sync.Mutex
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached = append(reached, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/legion/v1/state":
			w.WriteHeader(http.StatusTeapot)
			_, _ = io.WriteString(w, `{"issues":{}}`)
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"taken":true}`)
		}
	}))
	t.Cleanup(daemon.Close)
	calls := newRigCalls(t, daemon.URL)
	proxy := httptest.NewServer(interceptReport(calls))
	t.Cleanup(proxy.Close)
	send := func(method, path, body string) (int, string) {
		t.Helper()
		request, err := http.NewRequest(method, proxy.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer response.Body.Close()
		answer, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(answer)
	}

	status, answer := send(http.MethodPost, rigDefectRoute, `{"issue":"ACME-1","defect":"a defect","head":"`+stepRecordsHead+`"}`)
	var made map[string]any
	if err := json.Unmarshal([]byte(answer), &made); err != nil || status != http.StatusOK ||
		made["defect"] != "d-intercepted" || made["state"] != "recorded" || made["nextChange"] != float64(1) || len(made) != 3 {
		t.Errorf("the report through the proxy = %d %s, want 200 with the made answer", status, answer)
	}
	if got := calls.all(); len(got) != 0 {
		t.Errorf("the recording handler kept %+v of the intercepted report, want nothing", got)
	}

	if status, _ := send(http.MethodGet, "/legion/v1/state", ""); status != http.StatusTeapot {
		t.Errorf("GET /legion/v1/state through the proxy = %d, want the daemon's 418", status)
	}
	if status, _ := send(http.MethodPost, "/legion/v1/handoff/complete", `{"summary":"done"}`); status != http.StatusCreated {
		t.Errorf("POST /legion/v1/handoff/complete through the proxy = %d, want the daemon's 201", status)
	}
	kept := calls.all()
	got := make([]string, len(kept))
	for place, call := range kept {
		got[place] = call.Method + " " + call.Path + " " + http.StatusText(call.Status)
	}
	want := []string{"GET /legion/v1/state I'm a teapot", "POST /legion/v1/handoff/complete Created"}
	if !slices.Equal(got, want) {
		t.Errorf("the recording handler kept %v, want %v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(reached, http.MethodPost+" "+rigDefectRoute) {
		t.Errorf("the daemon was sent the intercepted report: %v", reached)
	}
}

// rigOwnRequest is a daemon request of the moment retro, every path absolute.
func rigOwnRequest() map[string]any {
	return map[string]any{
		"moment": "retro", "role": "implementer", "issue": "ACME-1", "steps": []string{"implementer", "reviewer", "acceptance:owner"},
		"world": "/work/worlds/forward-only/retro-defect", "dispatchUrl": "http://127.0.0.1:4100", "dispatchTokenFile": "/work/run/dispatch-token",
		"envoyUrl": "http://127.0.0.1:4200", "envoyTokenFile": "/work/services/envoy-token", "natsUrl": "nats://127.0.0.1:4222",
		"postgresUrl": "postgres://ci@127.0.0.1:5432/postgres", "ghLog": "/work/run/calls.jsonl", "omp": "/work/bin/omp",
		"home": "/work/run/home", "profile": "rig-label", "paneLogs": "/work/run/panes", "paneDaemon": "label", "runDir": "/work/run",
	}
}

// A request names every field TestRigDaemonLive handles and nothing more: a field it does not
// handle is refused by its name, and so is a field it needs that the request lacks.
func TestRigDaemonLiveRefusesAnUnknownField(t *testing.T) {
	t.Parallel()
	encode := func(fields map[string]any) []byte {
		t.Helper()
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	own := rigOwnRequest()
	req, err := decodeRigDaemonRequest(encode(own))
	if err != nil {
		t.Fatalf("the own request is refused: %v", err)
	}
	if req.Moment != "retro" || req.Issue != "ACME-1" || req.RunDir != "/work/run" || req.PaneDaemon != "label" {
		t.Errorf("the own request reads as %+v", req)
	}
	guided := rigOwnRequest()
	guided["planGuidanceFile"] = "/rig/guidance.md"
	if _, err := decodeRigDaemonRequest(encode(guided)); err == nil || !strings.Contains(err.Error(), `"planGuidanceFile"`) {
		t.Errorf("the request with planGuidanceFile is answered %v, want a refusal naming planGuidanceFile", err)
	}
	lacking := rigOwnRequest()
	delete(lacking, "runDir")
	if _, err := decodeRigDaemonRequest(encode(lacking)); err == nil || !strings.Contains(err.Error(), "runDir") {
		t.Errorf("the request without runDir is answered %v, want a refusal naming runDir", err)
	}
	relative := rigOwnRequest()
	relative["paneLogs"] = "panes"
	if _, err := decodeRigDaemonRequest(encode(relative)); err == nil || !strings.Contains(err.Error(), "paneLogs") {
		t.Errorf("the request with a relative paneLogs is answered %v, want a refusal naming paneLogs", err)
	}
	unknownDaemon := rigOwnRequest()
	unknownDaemon["paneDaemon"] = "standin"
	if _, err := decodeRigDaemonRequest(encode(unknownDaemon)); err == nil || !strings.Contains(err.Error(), "paneDaemon") {
		t.Errorf("the request with paneDaemon standin is answered %v, want a refusal naming paneDaemon", err)
	}
}

// TestRigDaemonLive with no request fails naming RIG_TEXTS_REQUEST, and with a request and no
// file to write, naming the file's variable.
func TestRigDaemonLiveNeedsARequest(t *testing.T) {
	t.Parallel()
	env := func(set map[string]string) func(string) string {
		return func(name string) string { return set[name] }
	}
	if _, err := rigDaemonLiveFiles(env(nil)); err == nil || !strings.Contains(err.Error(), "RIG_TEXTS_REQUEST") {
		t.Errorf("no request is answered %v, want a refusal naming RIG_TEXTS_REQUEST", err)
	}
	all := map[string]string{"RIG_TEXTS_REQUEST": "/run/request.json", "RIG_TEXTS_READY": "/run/ready.json",
		"RIG_TEXTS_STATUS": "/run/status.json", "RIG_TEXTS_RECORD": "/run/record.json"}
	files, err := rigDaemonLiveFiles(env(all))
	if err != nil || files.request != "/run/request.json" || files.ready != "/run/ready.json" || files.status != "/run/status.json" || files.record != "/run/record.json" {
		t.Errorf("the four files read as %+v, %v", files, err)
	}
	for _, name := range []string{"RIG_TEXTS_READY", "RIG_TEXTS_STATUS", "RIG_TEXTS_RECORD"} {
		lacking := map[string]string{}
		for key, value := range all {
			if key != name {
				lacking[key] = value
			}
		}
		if _, err := rigDaemonLiveFiles(env(lacking)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("a request without %s is answered %v, want a refusal naming it", name, err)
		}
	}
}

// rigResultsStandIn is the run's Dispatch as the passes of a world read it: the results of one
// issue's checks, and the results route, which keeps each result it is sent on its check.
type rigResultsStandIn struct {
	mu     sync.Mutex
	checks []map[string]any
	posted []map[string]any
}

func (s *rigResultsStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer rig-agent-token" || r.URL.Path != "/api/v1/issues/ACME-1/results" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]any{"checks": s.checks})
		return
	}
	var result map[string]any
	if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.posted = append(s.posted, result)
	for _, check := range s.checks {
		if check["block_id"] == result["check"] {
			check["results"] = append(check["results"].([]any), map[string]any{"verdict": result["verdict"], "commit_sha": result["commit"]})
		}
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, `{"result":{"id":"r-1"}}`)
}

// A world's passes are recorded at the head, and a before_merge check the world names no pass of
// is a refusal naming it: the run then writes no <ready> and launches no pane. A check after the
// merge needs no pass.
func TestRigDaemonLiveRefusesAHeadWithoutItsPasses(t *testing.T) {
	t.Parallel()
	head := stepRecordsHead
	check := func(id, when string, results ...any) map[string]any {
		return map[string]any{"block_id": id, "when": when, "surface": "cli", "run": "bun test " + id, "results": append([]any{}, results...)}
	}
	standIn := &rigResultsStandIn{checks: []map[string]any{
		check("c-unit", "before_merge"),
		check("c-lint", "before_merge", map[string]any{"verdict": "pass", "commit_sha": strings.Repeat("0", 40)}),
		check("c-after", "after_merge"),
	}}
	server := httptest.NewServer(standIn)
	t.Cleanup(server.Close)
	d := rigDispatch{base: server.URL, token: "rig-agent-token"}

	err := passAtHead(d, "ACME-1", head, []string{"c-unit"})
	if err == nil || !strings.Contains(err.Error(), "c-lint") || strings.Contains(err.Error(), "c-unit") || strings.Contains(err.Error(), "c-after") {
		t.Errorf("the passes of c-unit alone are answered %v, want a refusal naming c-lint alone", err)
	}
	if len(standIn.posted) != 1 || standIn.posted[0]["check"] != "c-unit" || standIn.posted[0]["commit"] != head ||
		standIn.posted[0]["verdict"] != "pass" || standIn.posted[0]["run"] != "bun test c-unit" {
		t.Errorf("Dispatch was sent %v, want one pass of c-unit at %s with its run", standIn.posted, head)
	}
	if err := passAtHead(d, "ACME-1", head, []string{"c-lint"}); err != nil {
		t.Errorf("the pass of c-lint after c-unit's is answered %v, want none", err)
	}
	if err := passAtHead(d, "ACME-1", head, []string{"c-missing"}); err == nil || !strings.Contains(err.Error(), "c-missing") {
		t.Errorf("a pass of a check the document lacks is answered %v, want a refusal naming c-missing", err)
	}
}
