package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The parts of TestRigDaemonLive (rig_daemon_live_test.go) that need no Oh My Pi, in a file with
// no build tag so that CI's daemon-go job runs their tests: the runtime that routes each launch by
// its claim (rigRuntime), the intercept control's proxy (interceptReport), the request and its
// files (decodeRigDaemonRequest, rigDaemonLiveFiles), the world's file (readRigWorld) and the
// passes the world's head needs before a pane launches (passAtHead).

// rigRuntime is the one runtime of TestRigDaemonLive's daemon, which routes each launch by its
// claim's token. The moment's claim goes to second once its real launch is due (switchMoment), and
// every other launch, the moment's before then among them, goes to first. Every call about a
// claim's process goes to the runtime that made the claim's newest launch (holder), and the sweep
// passes on what each runtime says only of the claims it holds.
type rigRuntime struct {
	first, second runtime.Runtime
	moment        claim.Token
	mu            sync.Mutex
	switched      bool
	launched      map[claim.Token]runtime.Runtime
}

var _ runtime.Runtime = (*rigRuntime)(nil)

func newRigRuntime(first, second runtime.Runtime, moment claim.Token) *rigRuntime {
	return &rigRuntime{first: first, second: second, moment: moment, launched: map[claim.Token]runtime.Runtime{}}
}

// switchMoment gives every later launch of the moment's claim to the second runtime.
func (r *rigRuntime) switchMoment() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.switched = true
}

// launcher is the runtime token's next launch goes to.
func (r *rigRuntime) launcher(token claim.Token) runtime.Runtime {
	r.mu.Lock()
	defer r.mu.Unlock()
	if token == r.moment && r.switched {
		return r.second
	}
	return r.first
}

// holder is the runtime that made token's newest launch, and, for a claim with no launch yet,
// the runtime its next launch goes to.
func (r *rigRuntime) holder(token claim.Token) runtime.Runtime {
	r.mu.Lock()
	held, ok := r.launched[token]
	r.mu.Unlock()
	if ok {
		return held
	}
	return r.launcher(token)
}

// launch is one launch of token by rt, which holds the claim from then on when it starts.
func (r *rigRuntime) launch(token claim.Token, rt runtime.Runtime, start func() (runtime.Locator, error)) (runtime.Locator, error) {
	loc, err := start()
	if err == nil {
		r.mu.Lock()
		r.launched[token] = rt
		r.mu.Unlock()
	}
	return loc, err
}

func (r *rigRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	rt := r.launcher(spec.Claim)
	return r.launch(spec.Claim, rt, func() (runtime.Locator, error) { return rt.Spawn(ctx, spec) })
}

func (r *rigRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	rt := r.launcher(spec.Claim)
	return r.launch(spec.Claim, rt, func() (runtime.Locator, error) { return rt.Resume(ctx, prev, spec) })
}

func (r *rigRuntime) Suspend(ctx context.Context, loc runtime.Locator) error {
	return r.holder(loc.Claim).Suspend(ctx, loc)
}

func (r *rigRuntime) Release(ctx context.Context, k runtime.Known) error {
	return r.holder(k.Claim).Release(ctx, k)
}

func (r *rigRuntime) Probe(ctx context.Context, loc runtime.Locator) (runtime.Observation, error) {
	return r.holder(loc.Claim).Probe(ctx, loc)
}

// Observe is both runtimes' sweeps as one, each passing on only what it says of a claim it holds:
// a claim the daemon launched again on the other runtime is that runtime's to report.
func (r *rigRuntime) Observe(ctx context.Context) (<-chan runtime.Observation, error) {
	out := make(chan runtime.Observation)
	var sweeps sync.WaitGroup
	for _, rt := range []runtime.Runtime{r.first, r.second} {
		in, err := rt.Observe(ctx)
		if err != nil {
			return nil, err
		}
		sweeps.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case o, open := <-in:
					if !open {
						return
					}
					if r.holder(o.Locator.Claim) != rt {
						continue
					}
					select {
					case out <- o:
					case <-ctx.Done():
						return
					}
				}
			}
		})
	}
	go func() {
		sweeps.Wait()
		close(out)
	}()
	return out, nil
}

// ReconcileOrphans asks both runtimes, each of which ends only what it started itself.
func (r *rigRuntime) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	return errors.Join(r.first.ReconcileOrphans(ctx, known, grace), r.second.ReconcileOrphans(ctx, known, grace))
}

func (r *rigRuntime) AdoptWorkingCopy(ctx context.Context, loc runtime.Locator, id runtime.GitIdentity) error {
	return r.holder(loc.Claim).AdoptWorkingCopy(ctx, loc, id)
}

func (r *rigRuntime) StartAtBranch(ctx context.Context, loc runtime.Locator, at runtime.StartAt) (runtime.StartedAt, error) {
	return r.holder(loc.Claim).StartAtBranch(ctx, loc, at)
}

// ProvisionsWorkspaces is whether both runtimes make each claim's working directory themselves,
// which the fake of the rig and the local runtime both do: the daemon then clones nothing.
func (r *rigRuntime) ProvisionsWorkspaces() bool {
	return r.first.ProvisionsWorkspaces() && r.second.ProvisionsWorkspaces()
}

func (r *rigRuntime) ProvisionBound() time.Duration {
	return max(r.first.ProvisionBound(), r.second.ProvisionBound())
}

func (r *rigRuntime) LaunchBound() time.Duration {
	return max(r.first.LaunchBound(), r.second.LaunchBound())
}

func (r *rigRuntime) PodDrift(ctx context.Context, k runtime.Known) ([]string, error) {
	return r.holder(k.Claim).PodDrift(ctx, k)
}

func (r *rigRuntime) ReplacePod(ctx context.Context, k runtime.Known, due func(context.Context) error) error {
	return r.holder(k.Claim).ReplacePod(ctx, k, due)
}

func (r *rigRuntime) CleanupTree(ctx context.Context, tree string) error {
	return errors.Join(r.first.CleanupTree(ctx, tree), r.second.CleanupTree(ctx, tree))
}

// rigInterceptedReport is the answer the intercept control's proxy gives report_defect itself.
const rigInterceptedReport = `{"defect": "d-intercepted", "state": "recorded", "nextChange": 1}`

// interceptReport is the proxy of the request's paneDaemon `intercept-report`, the path check's
// control: it answers POST /legion/v1/defects/report itself with rigInterceptedReport, and hands
// every other request to next, the recording handler in front of the daemon. So the pane's
// registration, readiness and task are the label daemon's, and only the report goes elsewhere.
func interceptReport(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != rigDefectRoute {
			next.ServeHTTP(w, req)
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, rigInterceptedReport)
	})
}

// rigDaemonRequest is the request of `label-texts.sh daemon` (RigDaemonRequest in
// packages/pi-legion/scripts/skill-scenarios/scored-runs-contracts.ts).
type rigDaemonRequest struct {
	Moment            string   `json:"moment"`
	Role              string   `json:"role"`
	Issue             string   `json:"issue"`
	Steps             []string `json:"steps"`
	World             string   `json:"world"`
	DispatchURL       string   `json:"dispatchUrl"`
	DispatchTokenFile string   `json:"dispatchTokenFile"`
	EnvoyURL          string   `json:"envoyUrl"`
	EnvoyTokenFile    string   `json:"envoyTokenFile"`
	NatsURL           string   `json:"natsUrl"`
	PostgresURL       string   `json:"postgresUrl"`
	GHLog             string   `json:"ghLog"`
	Omp               string   `json:"omp"`
	Home              string   `json:"home"`
	Profile           string   `json:"profile"`
	PaneLogs          string   `json:"paneLogs"`
	PaneDaemon        string   `json:"paneDaemon"`
	RunDir            string   `json:"runDir"`
}

// rigMomentRoles is the role of the real pane at each moment (C4's transfer table).
var rigMomentRoles = map[string]claim.Role{"retro": claim.RoleImplementer, "merging": claim.RoleMerger, "notice": claim.RoleArchitect}

// decodeRigDaemonRequest reads a daemon request, refusing by its name a field this test does not
// handle, a field it needs that the request lacks, a value it does not know, and a path the test
// binary would read from its own package directory.
func decodeRigDaemonRequest(data []byte) (rigDaemonRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var req rigDaemonRequest
	if err := decoder.Decode(&req); err != nil {
		return rigDaemonRequest{}, err
	}
	if decoder.More() {
		return rigDaemonRequest{}, errors.New("the request holds more than one JSON value")
	}
	fields := []struct{ name, value string }{
		{"moment", req.Moment}, {"role", req.Role}, {"issue", req.Issue}, {"world", req.World}, {"dispatchUrl", req.DispatchURL},
		{"dispatchTokenFile", req.DispatchTokenFile}, {"envoyUrl", req.EnvoyURL}, {"envoyTokenFile", req.EnvoyTokenFile},
		{"natsUrl", req.NatsURL}, {"postgresUrl", req.PostgresURL}, {"ghLog", req.GHLog}, {"omp", req.Omp}, {"home", req.Home},
		{"profile", req.Profile}, {"paneLogs", req.PaneLogs}, {"paneDaemon", req.PaneDaemon}, {"runDir", req.RunDir},
	}
	for _, field := range fields {
		if field.value == "" {
			return rigDaemonRequest{}, fmt.Errorf("the request names no %s", field.name)
		}
	}
	if len(req.Steps) == 0 {
		return rigDaemonRequest{}, errors.New("the request names no steps")
	}
	role, known := rigMomentRoles[req.Moment]
	switch {
	case !known:
		return rigDaemonRequest{}, fmt.Errorf("moment is %q, want retro, merging or notice", req.Moment)
	case req.Role != string(role):
		return rigDaemonRequest{}, fmt.Errorf("role is %q, and the moment %s is the %s's", req.Role, req.Moment, role)
	case req.PaneDaemon != "label" && req.PaneDaemon != "intercept-report":
		return rigDaemonRequest{}, fmt.Errorf("paneDaemon is %q, want label or intercept-report", req.PaneDaemon)
	}
	for _, path := range []struct{ name, value string }{
		{"world", req.World}, {"dispatchTokenFile", req.DispatchTokenFile}, {"envoyTokenFile", req.EnvoyTokenFile}, {"ghLog", req.GHLog},
		{"omp", req.Omp}, {"home", req.Home}, {"paneLogs", req.PaneLogs}, {"runDir", req.RunDir},
	} {
		if !filepath.IsAbs(path.value) {
			return rigDaemonRequest{}, fmt.Errorf("%s is %q, want an absolute path", path.name, path.value)
		}
	}
	return req, nil
}

// rigDaemonFiles is the four files of `label-texts.sh daemon`: the request it reads, and the
// ready, status and record files it writes.
type rigDaemonFiles struct {
	request, ready, status, record string
}

// rigDaemonLiveFiles reads the four files' paths from getenv. TestRigDaemonLive has no own case,
// so a run with no request is refused, naming RIG_TEXTS_REQUEST, and so is a request with no file
// to write, naming that file's variable.
func rigDaemonLiveFiles(getenv func(string) string) (rigDaemonFiles, error) {
	files := rigDaemonFiles{request: getenv("RIG_TEXTS_REQUEST"), ready: getenv("RIG_TEXTS_READY"),
		status: getenv("RIG_TEXTS_STATUS"), record: getenv("RIG_TEXTS_RECORD")}
	if files.request == "" {
		return rigDaemonFiles{}, errors.New("RIG_TEXTS_REQUEST is unset: TestRigDaemonLive has no own case, and runs only as `label-texts.sh daemon <request> <ready> <status> <record>` runs it")
	}
	for _, file := range []struct{ name, value string }{
		{"RIG_TEXTS_READY", files.ready}, {"RIG_TEXTS_STATUS", files.status}, {"RIG_TEXTS_RECORD", files.record},
	} {
		if file.value == "" {
			return rigDaemonFiles{}, fmt.Errorf("RIG_TEXTS_REQUEST names a request and %s is unset: name the file the run writes there", file.name)
		}
	}
	return files, nil
}

// rigWorldFile is the fields of a world's world.json that TestRigDaemonLive reads; the rest are
// the rig's and the scorer's. Passes are the block ids of the checks whose pass the test records
// at the head, and Reported the defect texts the fake implementer reports in its retro once the
// architect's real pane is ready (W5).
type rigWorldFile struct {
	Daemon     string   `json:"daemon"`
	Issue      string   `json:"issue"`
	Title      string   `json:"title"`
	Repository string   `json:"repository"`
	Pull       int      `json:"pull"`
	Passes     []string `json:"passes"`
	Reported   []string `json:"reported"`
}

// readRigWorld reads world.json in the world's directory, and refuses a world this test cannot
// run for req: one whose daemon is not the label's, one of another issue, one on a repository
// other than the one the rig's GitHub stand-in serves, and one with no pull request.
func readRigWorld(req rigDaemonRequest) (rigWorldFile, error) {
	path := filepath.Join(req.World, "world.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return rigWorldFile{}, err
	}
	var world rigWorldFile
	if err := json.Unmarshal(data, &world); err != nil {
		return rigWorldFile{}, fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case world.Daemon != "label":
		return rigWorldFile{}, fmt.Errorf("%s names the daemon %q, and only a world of the label's daemon runs here", path, world.Daemon)
	case world.Issue != req.Issue:
		return rigWorldFile{}, fmt.Errorf("%s is of the issue %q, and the request of %s", path, world.Issue, req.Issue)
	case world.Repository != liveRepo:
		return rigWorldFile{}, fmt.Errorf("%s is on the repository %q, and the rig's GitHub stand-in serves %s alone", path, world.Repository, liveRepo)
	case world.Pull <= 0 || world.Title == "":
		return rigWorldFile{}, fmt.Errorf("%s names no pull request number or no title", path)
	}
	return world, nil
}

// rigChecksSession is the session the world's passes are recorded as.
const rigChecksSession = "rig-checks"

// rigDispatch is the run's Dispatch, read and written with the agent's bearer, the one credential
// the request gives.
type rigDispatch struct {
	base, token string
}

// call sends one request with body, nil for none, decodes the answer into into when it is not
// nil, and fails unless Dispatch answers want.
func (d rigDispatch) call(method, path string, body, into any, want int) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, d.base+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+d.token)
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != want {
		return fmt.Errorf("%s %s = %d %s, want %d", method, path, response.StatusCode, answer, want)
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(answer, into)
}

// rigCheck is one check of an issue as GET /api/v1/issues/{key}/results answers it, with what a
// pass at a head needs of its results.
type rigCheck struct {
	BlockID string `json:"block_id"`
	When    string `json:"when"`
	Surface string `json:"surface"`
	Run     string `json:"run"`
	Results []struct {
		Verdict    string          `json:"verdict"`
		CommitSHA  string          `json:"commit_sha"`
		NotCounted string          `json:"not_counted"`
		Retracted  json.RawMessage `json:"retracted"`
	} `json:"results"`
}

// passedAt is whether the check holds a pass at head that counts: one no retraction stands
// against and that Dispatch counts.
func (c rigCheck) passedAt(head string) bool {
	return slices.ContainsFunc(c.Results, func(result struct {
		Verdict    string          `json:"verdict"`
		CommitSHA  string          `json:"commit_sha"`
		NotCounted string          `json:"not_counted"`
		Retracted  json.RawMessage `json:"retracted"`
	}) bool {
		retracted := len(result.Retracted) > 0 && string(result.Retracted) != "null"
		return result.Verdict == "pass" && result.CommitSHA == head && result.NotCounted == "" && !retracted
	})
}

// checks is the issue's checks, as Dispatch answers its results now.
func (d rigDispatch) checks(issue string) ([]rigCheck, error) {
	var read struct {
		Checks []rigCheck `json:"checks"`
	}
	if err := d.call(http.MethodGet, "/api/v1/issues/"+issue+"/results", nil, &read, http.StatusOK); err != nil {
		return nil, err
	}
	return read.Checks, nil
}

// passAtHead records a pass at head of each check the world names in passes, as the session
// rigChecksSession in the role tester, then reads the issue's results again. A check of the world's
// passes the issue's Verification document lacks is an error, and so is each before_merge check
// with no pass at head, named: the retro's gate would send the task back for it, a cause that
// stays, so the run must launch no pane.
func passAtHead(d rigDispatch, issue, head string, passes []string) error {
	checks, err := d.checks(issue)
	if err != nil {
		return fmt.Errorf("read the results of %s: %w", issue, err)
	}
	for _, id := range passes {
		at := slices.IndexFunc(checks, func(c rigCheck) bool { return c.BlockID == id })
		if at < 0 {
			return fmt.Errorf("the world names a pass of %s, and the Verification document of %s holds no check %s", id, issue, id)
		}
		check := checks[at]
		if err := d.call(http.MethodPost, "/api/v1/issues/"+issue+"/results", map[string]any{
			"actor": map[string]string{"kind": "session", "id": rigChecksSession}, "check": id, "role": "tester",
			"verdict": "pass", "commit": head, "surface": check.Surface, "run": check.Run, "observed": "the world's pass at its head",
		}, nil, http.StatusCreated); err != nil {
			return fmt.Errorf("record the world's pass of %s at %s: %w", id, head, err)
		}
	}
	if checks, err = d.checks(issue); err != nil {
		return fmt.Errorf("read the results of %s again: %w", issue, err)
	}
	var unpassed []string
	for _, check := range checks {
		if check.When == "before_merge" && !check.passedAt(head) {
			unpassed = append(unpassed, check.BlockID)
		}
	}
	if len(unpassed) > 0 {
		return fmt.Errorf("the before_merge checks %s of %s have no pass at the head %s: the retro's gate would send the task back for them, so the run launches no pane",
			strings.Join(unpassed, ", "), issue, head)
	}
	return nil
}

// acmeToken is the claim token of role on issue in the rig's project ACME.
func acmeToken(t *testing.T, issue string, role claim.Role) claim.Token {
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
	moment, other := acmeToken(t, "ACME-1", claim.RoleImplementer), acmeToken(t, "ACME-1", claim.RoleReviewer)
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
