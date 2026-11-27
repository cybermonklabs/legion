package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/apifixture"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// pauseStartsAt is where a pause harness's clock starts.
var pauseStartsAt = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

// pauseClock is the pause service's clock, moved by the test alone.
type pauseClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *pauseClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *pauseClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// pauseHarness is the harness with the real pause service (internal/pause) over its own database,
// with a clock of its own, the service's JSON log, and the Dispatch the status route writes to.
type pauseHarness struct {
	*harness
	svc      *pause.Service
	clock    *pauseClock
	logs     *testlog.Buffer
	statuses *statusRecorder
}

// pauseCapable is a configuration that can hold a pause: runtime: kubernetes with the idle release
// on and a wake pacing of 8 a minute, as every production project runs (pause.Unavailable).
func pauseCapable() config.Config {
	return config.Config{Runtime: config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{IdleRelease: config.DefaultIdleRelease()}},
		WorkerWakesPerMinute: 8}
}

// newPauseHarness is a daemon of project LEGION whose API runs the real pause service, built with
// cfg, over the harness's own store. The service is handed no after-commit steps: the machines,
// the pod holds and the outbox lanes they drive are the daemon's, and this test is the routes'.
func newPauseHarness(t *testing.T, cfg config.Config) *pauseHarness {
	t.Helper()
	h := &pauseHarness{harness: newHarness(t), clock: &pauseClock{at: pauseStartsAt}, logs: &testlog.Buffer{},
		statuses: &statusRecorder{issues: map[string]dispatch.Issue{}}}
	svc, err := pause.New(pause.Options{Store: h.store, Records: record.NewStore(), Project: testDispatchProject, Config: cfg,
		Log: slog.New(slog.NewJSONHandler(h.logs, nil)), Now: h.clock.Now})
	if err != nil {
		t.Fatalf("build the pause service: %v", err)
	}
	h.svc = svc
	h.serveWith(svc)
	return h
}

// serveWith replaces the harness's server with one over the same store, machines and boot tokens
// whose pause service is svc; nil is a daemon with no workflow, which has none.
func (h *pauseHarness) serveWith(svc PauseService) {
	h.t.Helper()
	h.handler = NewServer("127.0.0.1", 8437, h.pauseOptions(svc)).Handler
}

// pauseOptions is the daemon serveWith serves: the harness's store, machines and boot tokens,
// with svc as its pause service. placePodCallers starts from it too.
func (h *pauseHarness) pauseOptions(svc PauseService) Options {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		OperatorToken: testOperatorToken, Controller: h.store, Grants: credential.New(nil), Log: quiet,
		Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store,
		Handlers: []intake.Handler{&factRecorder{}}, Dispatch: h.statuses, Pause: svc,
	}
}

// controllerSession registers the project's controller as the session the constant
// controllerSession names, as `legion controller start` and the session's registration do, and
// answers a grant that session minted.
func (h *harness) controllerSession() string {
	h.t.Helper()
	registration := h.registeredController(h.mintedSecret(), controllerSession)
	recorder := h.controllerSessionGrant(controllerSession, registration.Secret)
	if recorder.Code != http.StatusOK {
		h.t.Fatalf("the controller session's grant = %d; body %s", recorder.Code, recorder.Body)
	}
	var grant GrantResponse
	decodeInto(h.t, recorder, &grant)
	return grant.GrantID
}

// The four routes' paths.
const (
	controllerPauseRoute  = "/legion/v1/project/pause"
	controllerResumeRoute = "/legion/v1/project/resume"
	operatorPauseRoute    = "/legion/v1/operator/project/pause"
	operatorResumeRoute   = "/legion/v1/operator/project/resume"
)

// beganPause is the pause a 200 answer of the pause route began.
func beganPause(t *testing.T, recorder *httptest.ResponseRecorder) PauseView {
	t.Helper()
	var answer ProjectPauseResponse
	decodeInto(t, recorder, &answer)
	return answer.Pause
}

// pauseOK sends a pause or resume and requires a 200, answering the response.
func (h *pauseHarness) pauseOK(target string, body any, header http.Header) *httptest.ResponseRecorder {
	h.t.Helper()
	recorder := h.request(http.MethodPost, target, body, header)
	if recorder.Code != http.StatusOK {
		h.t.Fatalf("POST %s = %d, want 200; body %s", target, recorder.Code, recorder.Body)
	}
	return recorder
}

// pauseRefused sends a request and requires the refusal status and code, answering its body.
func (h *pauseHarness) pauseRefused(target string, body any, header http.Header, status int, code string) PauseFailure {
	h.t.Helper()
	return refusalServed(h.t, h.request(http.MethodPost, target, body, header), target, status, code)
}

// refusalServed requires recorder to answer the POST of target with a refusal of status coded
// code with its sentence, and answers the refusal, decoded out of the recorder. pauseRefused and
// pauseRefusalServed both check a refusal through it.
func refusalServed(t *testing.T, recorder *httptest.ResponseRecorder, target string, status int, code string) PauseFailure {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("POST %s = %d, want %d; body %s", target, recorder.Code, status, recorder.Body)
	}
	var refused PauseFailure
	decodeInto(t, recorder, &refused)
	if refused.Code != code || refused.Error == "" {
		t.Fatalf("POST %s refused %+v, want code %s and a sentence", target, refused, code)
	}
	return refused
}

// bearer is the operator's Authorization header.
var bearer = http.Header{"Authorization": {"Bearer " + testOperatorToken}}

// pauseCount is how many pauses project_pauses holds.
func (h *pauseHarness) pauseCount() int {
	h.t.Helper()
	var n int
	if err := h.store.Pool().QueryRow(context.Background(), `select count(*) from project_pauses`).Scan(&n); err != nil {
		h.t.Fatalf("count the pauses: %v", err)
	}
	return n
}

// pauseNotices is every controller notice of the pause's two kinds the outbox holds, in order.
func (h *pauseHarness) pauseNotices() []record.ControllerNotice {
	h.t.Helper()
	rows, err := h.store.Pool().Query(context.Background(), `select issue, payload from outbox where kind = $1 order by id`,
		string(record.OutboxKindControllerNotice))
	if err != nil {
		h.t.Fatalf("read the controller notices: %v", err)
	}
	defer rows.Close()
	var notices []record.ControllerNotice
	for rows.Next() {
		row := record.OutboxRow{Kind: record.OutboxKindControllerNotice}
		if err := rows.Scan(&row.Issue, &row.Payload); err != nil {
			h.t.Fatal(err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			h.t.Fatalf("decode the notice %s: %v", row.Payload, err)
		}
		notice := payload.(record.ControllerNotice)
		if notice.Kind == record.ProjectPausedNotice || notice.Kind == record.ProjectResumedNotice {
			notices = append(notices, notice)
		}
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return notices
}

// logLines is every JSON line of the service's log whose message is msg.
func (h *pauseHarness) logLines(msg string) []map[string]any {
	h.t.Helper()
	var found []map[string]any
	for _, text := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		if text == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			h.t.Fatalf("log line %q is no JSON: %v", text, err)
		}
		if line["msg"] == msg {
			found = append(found, line)
		}
	}
	return found
}

const (
	pausedLine  = "pause: the project is paused"
	resumedLine = "pause: the project is resumed"
)

// wantActs holds the service's log to one line for each pause and each resume, the resume's naming
// who ended its pause.
func (h *pauseHarness) wantActs(pauses, resumes int, endedBy, endedBySession string) {
	h.t.Helper()
	if got := h.logLines(pausedLine); len(got) != pauses {
		h.t.Fatalf("the daemon logged %d pause lines, want %d: %s", len(got), pauses, h.logs)
	}
	got := h.logLines(resumedLine)
	if len(got) != resumes {
		h.t.Fatalf("the daemon logged %d resume lines, want %d: %s", len(got), resumes, h.logs)
	}
	if resumes > 0 {
		last := got[len(got)-1]
		if last["by"] != endedBy || last["by_session"] != endedBySession {
			h.t.Fatalf("the resume line names %v (session %v) as who ended the pause, want %s (session %q): %v",
				last["by"], last["by_session"], endedBy, endedBySession, last)
		}
	}
}

// TestPauseRoutes is the API half of check k5 of dispatch://INFRA-3759: the pause and resume
// routes over the real pause service and a database of their own.
func TestPauseRoutes(t *testing.T) {
	controller := PauseActorView{Kind: "controller", Session: "ses_controller"}
	operator := PauseActorView{Kind: "operator"}

	// The controller's grant pauses and resumes the project, neither call names a project, `by`
	// names the controller's session, the daemon logs one line for each, the resume's naming who
	// ended the pause, and the controller's own acts tell it nothing.
	t.Run("the controller's grant pauses and resumes, naming its session", func(t *testing.T) {
		h := newPauseHarness(t, pauseCapable())
		grant := h.controllerSession()

		named := h.request(http.MethodPost, controllerPauseRoute, `{"grantId":"`+grant+`","reason":"an outage","project":"LEGION"}`, nil)
		if named.Code != http.StatusBadRequest {
			t.Fatalf("a pause naming a project = %d, want 400: the route takes none; body %s", named.Code, named.Body)
		}
		const reason = "the model provider is refusing every turn"
		began := beganPause(t, h.pauseOK(controllerPauseRoute, ProjectPauseRequest{GrantID: grant, Reason: reason}, nil))
		if began.ID == 0 || began.By != controller || began.Reason != reason || !began.Since.Equal(pauseStartsAt) || began.EndedAt != nil {
			t.Fatalf("the pause = %+v, want an open pause by the controller's session at %s for %q", began, pauseStartsAt, reason)
		}
		if id, open := h.svc.Paused(); !open || id != began.ID {
			t.Fatalf("the flag reads %d, %t after the pause, want pause %d", id, open, began.ID)
		}

		h.clock.advance(20 * time.Minute)
		var resumed ProjectResumeResponse
		decodeInto(t, h.pauseOK(controllerResumeRoute, ProjectResumeRequest{GrantID: grant}, nil), &resumed)
		if resumed.Pause.ID != began.ID || resumed.Pause.EndedBy == nil || *resumed.Pause.EndedBy != controller ||
			resumed.Pause.EndedAt == nil || !resumed.Pause.EndedAt.Equal(pauseStartsAt.Add(20*time.Minute)) {
			t.Fatalf("the resume answered %+v, want pause %d ended by the controller's session 20 minutes on", resumed.Pause, began.ID)
		}
		if _, open := h.svc.Paused(); open {
			t.Fatal("the flag still reads a pause after the resume")
		}
		if notices := h.pauseNotices(); len(notices) != 0 {
			t.Fatalf("the controller's own pause and resume queued %+v, want no notice", notices)
		}
		h.wantActs(1, 1, "controller", "ses_controller")
	})

	// The operator's routes record the operator, and each act queues one notice to the controller
	// naming who, when and why.
	t.Run("the operator's routes record the operator and tell the controller", func(t *testing.T) {
		h := newPauseHarness(t, pauseCapable())
		h.controllerSession()

		const reason = "a provider outage"
		began := beganPause(t, h.pauseOK(operatorPauseRoute, OperatorPauseRequest{Reason: reason}, bearer))
		if began.By != operator || began.Reason != reason {
			t.Fatalf("the operator's pause = %+v, want one by the operator for %q", began, reason)
		}
		h.clock.advance(5 * time.Minute)
		var resumed ProjectResumeResponse
		decodeInto(t, h.pauseOK(operatorResumeRoute, nil, bearer), &resumed)
		if resumed.Pause.ID != began.ID || resumed.Pause.EndedBy == nil || *resumed.Pause.EndedBy != operator {
			t.Fatalf("the operator's resume answered %+v, want pause %d ended by the operator", resumed.Pause, began.ID)
		}

		notices := h.pauseNotices()
		if len(notices) != 2 {
			t.Fatalf("the operator's pause and resume queued %d notices, want one each: %+v", len(notices), notices)
		}
		pausedNotice, resumedNotice := notices[0], notices[1]
		if pausedNotice.Kind != record.ProjectPausedNotice || pausedNotice.Pause == nil || pausedNotice.Pause.ID != began.ID ||
			pausedNotice.Pause.By.Kind != record.PauseByOperator || !pausedNotice.Pause.At.Equal(pauseStartsAt) || pausedNotice.Pause.Reason != reason {
			t.Fatalf("the pause's notice = %+v, want project-paused naming the operator, %s and %q", pausedNotice, pauseStartsAt, reason)
		}
		if resumedNotice.Kind != record.ProjectResumedNotice || resumedNotice.Pause == nil || resumedNotice.Pause.ID != began.ID ||
			resumedNotice.Pause.By.Kind != record.PauseByOperator || !resumedNotice.Pause.At.Equal(pauseStartsAt.Add(5*time.Minute)) {
			t.Fatalf("the resume's notice = %+v, want project-resumed naming the operator and when", resumedNotice)
		}
		h.wantActs(1, 1, "operator", "")

		// The operator's resume reads no body, and refuses one that names anything.
		if named := h.request(http.MethodPost, operatorResumeRoute, `{"project":"LEGION"}`, bearer); named.Code != http.StatusBadRequest {
			t.Fatalf("an operator resume naming a project = %d, want 400; body %s", named.Code, named.Body)
		}
	})

	// A grant the operator's token minted (`legion status`'s, the grants route's empty form) is a
	// controller grant, and the pause and resume through it are the operator's, which tell the
	// controller as the operator's routes do.
	t.Run("a grant the operator's token minted records the operator", func(t *testing.T) {
		h := newPauseHarness(t, pauseCapable())
		h.controllerSession()
		grant := controllerGrant(t, h.harness).GrantID

		began := beganPause(t, h.pauseOK(controllerPauseRoute, ProjectPauseRequest{GrantID: grant, Reason: "an operator's pause"}, nil))
		if began.By != operator {
			t.Fatalf("a pause through the operator's grant = %+v, want it the operator's", began)
		}
		var resumed ProjectResumeResponse
		decodeInto(t, h.pauseOK(controllerResumeRoute, ProjectResumeRequest{GrantID: grant}, nil), &resumed)
		if resumed.Pause.EndedBy == nil || *resumed.Pause.EndedBy != operator {
			t.Fatalf("a resume through the operator's grant = %+v, want it the operator's", resumed.Pause)
		}
		if notices := h.pauseNotices(); len(notices) != 2 {
			t.Fatalf("the pause and resume through the operator's grant queued %d notices, want one each: %+v", len(notices), notices)
		}
		h.wantActs(1, 1, "operator", "")
	})

	// An operator route with no token or a wrong one answers 403, and a phase worker's grant
	// answers 403 CONTROLLER_REQUIRED at both controller routes; none of them pauses anything.
	t.Run("a caller that is not the controller or the operator is refused", func(t *testing.T) {
		h := newPauseHarness(t, pauseCapable())
		for _, target := range []string{operatorPauseRoute, operatorResumeRoute} {
			for _, header := range []http.Header{nil, {"Authorization": {"Bearer not-the-operator"}}} {
				wantRefusal(t, h.request(http.MethodPost, target, OperatorPauseRequest{Reason: "x"}, header), http.StatusForbidden, invalidOperatorToken)
			}
		}
		worker := newLiveClaim(t, h.harness, "LEGION-208", claim.RoleImplementer)
		grant := worker.grant(t)
		h.pauseRefused(controllerPauseRoute, ProjectPauseRequest{GrantID: grant, Reason: "a worker's pause"}, nil, http.StatusForbidden, "CONTROLLER_REQUIRED")
		h.pauseRefused(controllerResumeRoute, ProjectResumeRequest{GrantID: grant}, nil, http.StatusForbidden, "CONTROLLER_REQUIRED")
		if n := h.pauseCount(); n != 0 {
			t.Fatalf("the refused calls left %d pauses, want none", n)
		}
		if _, open := h.svc.Paused(); open {
			t.Fatal("a refused call set the pause flag")
		}
	})

	// A blank reason is refused at both doors and pauses nothing; a second pause answers
	// PROJECT_ALREADY_PAUSED with the pause that stands; a resume of a project that runs answers
	// PROJECT_NOT_PAUSED, naming the last pause once one has ended; and the controller resumes a
	// pause the operator made.
	t.Run("the refusals of a pause and a resume", func(t *testing.T) {
		h := newPauseHarness(t, pauseCapable())
		grant := h.controllerSession()

		h.pauseRefused(controllerPauseRoute, ProjectPauseRequest{GrantID: grant, Reason: "  "}, nil, http.StatusBadRequest, pause.CodeReasonRequired)
		h.pauseRefused(operatorPauseRoute, OperatorPauseRequest{}, bearer, http.StatusBadRequest, pause.CodeReasonRequired)
		if n := h.pauseCount(); n != 0 {
			t.Fatalf("a blank reason left %d pauses, want none", n)
		}
		if refused := h.pauseRefused(controllerResumeRoute, ProjectResumeRequest{GrantID: grant}, nil, http.StatusConflict, pause.CodeNotPaused); refused.LastPause != nil {
			t.Fatalf("a resume before any pause named a last pause: %+v", refused)
		}

		began := beganPause(t, h.pauseOK(operatorPauseRoute, OperatorPauseRequest{Reason: "the operator's outage"}, bearer))
		standing := h.pauseRefused(controllerPauseRoute, ProjectPauseRequest{GrantID: grant, Reason: "the controller's outage"}, nil,
			http.StatusConflict, pause.CodeAlreadyPaused)
		if standing.Pause == nil || !reflect.DeepEqual(*standing.Pause, began) {
			t.Fatalf("the second pause's refusal named %+v, want the pause that stands, %+v", standing.Pause, began)
		}
		if n := h.pauseCount(); n != 1 {
			t.Fatalf("the second pause left %d pauses, want the first alone", n)
		}

		h.clock.advance(10 * time.Minute)
		var resumed ProjectResumeResponse
		decodeInto(t, h.pauseOK(controllerResumeRoute, ProjectResumeRequest{GrantID: grant}, nil), &resumed)
		if resumed.Pause.ID != began.ID || resumed.Pause.By != operator || resumed.Pause.EndedBy == nil || *resumed.Pause.EndedBy != controller {
			t.Fatalf("the controller's resume of the operator's pause answered %+v, want pause %d by the operator ended by the controller",
				resumed.Pause, began.ID)
		}
		again := h.pauseRefused(operatorResumeRoute, nil, bearer, http.StatusConflict, pause.CodeNotPaused)
		if again.LastPause == nil || !reflect.DeepEqual(*again.LastPause, resumed.Pause) {
			t.Fatalf("a resume of the running project named %+v, want the last pause, %+v", again.LastPause, resumed.Pause)
		}
		h.wantActs(1, 1, "controller", "ses_controller")
	})

	// While the project is paused, every request that would start a turn of a role but the
	// controller's answers PROJECT_PAUSED and changes nothing, and a stop, a suspension, an issue's
	// status and the daemon's own status still work.
	t.Run("while paused no turn starts but the controller's, and the rest still works", func(t *testing.T) {
		h := newPauseHarness(t, pauseCapable())
		grant := h.controllerSession()
		implementer, _ := h.launch("LEGION-208", claim.RoleImplementer)
		h.ready(implementer, "ses_implementer", 1)
		tester, _ := h.launch("LEGION-209", claim.RoleTester)
		h.ready(tester, "ses_tester", 1)
		route := func(token claim.Token, action string) string {
			return "/legion/v1/operator/claims/" + string(token) + "/" + action
		}
		if recorder := h.operator(http.MethodPost, route(tester, "suspend"), nil); recorder.Code != http.StatusOK {
			t.Fatalf("suspend the tester before the pause = %d; body %s", recorder.Code, recorder.Body)
		}
		reviewer, _ := h.launch("LEGION-210", claim.RoleReviewer)
		h.ready(reviewer, "ses_reviewer", 1)
		controllerClaim, _ := h.launchController()

		began := beganPause(t, h.pauseOK(operatorPauseRoute, OperatorPauseRequest{Reason: "a provider outage"}, bearer))
		before, err := h.store.Claims(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		calls := len(h.runtime.Calls())
		spawned, err := claim.NewToken(testProject, "LEGION-212", claim.RoleArchitect)
		if err != nil {
			t.Fatal(err)
		}

		refused := []struct {
			name   string
			target string
			body   any
			header http.Header
		}{
			{"the controller's compact", "/legion/v1/claims/compact", ClaimRequest{GrantID: grant, Issue: "LEGION-208", Role: string(claim.RoleImplementer)}, nil},
			{"the controller's fresh_session", "/legion/v1/claims/fresh-session", ClaimRequest{GrantID: grant, Issue: "LEGION-208", Role: string(claim.RoleImplementer)}, nil},
			{"legion claims resume", route(tester, "resume"), nil, bearer},
			{"legion claims retry", route(implementer, "retry"), nil, bearer},
			{"legion claims compact", route(implementer, "compact"), nil, bearer},
			{"legion claims spawn", "/legion/v1/operator/claims", SpawnRequest{Tree: "LEGION-212", Issue: "LEGION-212", Role: claim.RoleArchitect, Task: "Plan it."}, bearer},
			{"legion claims deliver", route(implementer, "deliver"), DeliverRequest{Task: "Do it."}, bearer},
		}
		for _, request := range refused {
			failure := h.pauseRefused(request.target, request.body, request.header, http.StatusConflict, projectPaused)
			if failure.Pause == nil || failure.Pause.ID != began.ID || !strings.Contains(failure.Error, "the project is paused") {
				t.Fatalf("%s refused %+v, want it to name pause %d", request.name, failure, began.ID)
			}
		}
		after, err := h.store.Claims(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(before) == 0 || !reflect.DeepEqual(after, before) {
			t.Fatalf("the %d refused requests changed the %d claims:\nbefore %+v\nafter  %+v", len(refused), len(before), before, after)
		}
		if got := len(h.runtime.Calls()); got != calls {
			t.Fatalf("the refused requests reached the runtime: %v", h.runtime.Methods()[calls:])
		}
		if _, made := h.supervisor.Machine(spawned); made {
			t.Fatalf("the refused spawn made %s", spawned)
		}
		if m, _ := h.supervisor.Machine(implementer); m.Claim().Pending != nil {
			t.Fatalf("the refused delivery left %s a task: %+v", implementer, m.Claim().Pending)
		}

		// The controller's own claim is no role the pause holds: its task is delivered.
		if recorder := h.operator(http.MethodPost, route(controllerClaim, "deliver"), DeliverRequest{Task: "Read the state."}); recorder.Code != http.StatusOK {
			t.Fatalf("deliver to the controller while paused = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
		// A suspension and a stop are no turn, and still work.
		for _, step := range []struct {
			token  claim.Token
			action string
			want   supervise.ClaimState
		}{{implementer, "suspend", supervise.StateSuspended}, {reviewer, "stop", supervise.StateRetired}} {
			recorder := h.operator(http.MethodPost, route(step.token, step.action), nil)
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s %s while paused = %d, want 200; body %s", step.action, step.token, recorder.Code, recorder.Body)
			}
			var got OperatorClaim
			decodeInto(t, recorder, &got)
			if got.State != string(step.want) {
				t.Fatalf("%s while paused left %s %s, want %s", step.action, step.token, got.State, step.want)
			}
		}
		// set_status, through the controller's grant and through the one `legion issue status`
		// mints with the operator's token, and `legion status <team>`'s health read.
		h.statuses.issues["LEGION-213"] = dispatch.Issue{Key: "LEGION-213"}
		for _, grantID := range []string{grant, controllerGrant(t, h.harness).GrantID} {
			recorder := h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{GrantID: grantID, Issue: "LEGION-213", Status: "backlog"}, nil)
			if recorder.Code != http.StatusOK {
				t.Fatalf("set_status while paused = %d, want 200; body %s", recorder.Code, recorder.Body)
			}
		}
		if len(h.statuses.writes) != 2 {
			t.Fatalf("Dispatch took %d status writes while paused, want 2: %+v", len(h.statuses.writes), h.statuses.writes)
		}
		if recorder := h.request(http.MethodGet, "/healthz", nil, nil); recorder.Code != http.StatusOK {
			t.Fatalf("healthz while paused = %d; body %s", recorder.Code, recorder.Body)
		}
		if _, open := h.svc.Paused(); !open {
			t.Fatal("the pause ended under the requests")
		}
	})

	// A claim of another project's issue, which the operator's spawn makes under this project's
	// token, is a session this daemon runs: while the project is paused every request that would
	// start a turn of it answers PROJECT_PAUSED and changes nothing, as for a role of the project's
	// own issue, and so does a spawn of another project's issue.
	t.Run("a claim of another project's issue is refused as every other claim is", func(t *testing.T) {
		h := newPauseHarness(t, pauseCapable())
		grant := h.controllerSession()
		architect, _ := h.launch("OTHER-5", claim.RoleArchitect)
		h.ready(architect, "ses_other_architect", 1)
		tester, _ := h.launch("OTHER-6", claim.RoleTester)
		h.ready(tester, "ses_other_tester", 1)
		route := func(token claim.Token, action string) string {
			return "/legion/v1/operator/claims/" + string(token) + "/" + action
		}
		if recorder := h.operator(http.MethodPost, route(tester, "suspend"), nil); recorder.Code != http.StatusOK {
			t.Fatalf("suspend the tester of OTHER-6 before the pause = %d; body %s", recorder.Code, recorder.Body)
		}

		began := beganPause(t, h.pauseOK(operatorPauseRoute, OperatorPauseRequest{Reason: "a provider outage"}, bearer))
		before, err := h.store.Claims(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		calls := len(h.runtime.Calls())
		for _, request := range []struct {
			name   string
			target string
			body   any
			header http.Header
		}{
			{"the controller's compact", "/legion/v1/claims/compact", ClaimRequest{GrantID: grant, Issue: "OTHER-5", Role: string(claim.RoleArchitect)}, nil},
			{"the controller's fresh_session", "/legion/v1/claims/fresh-session", ClaimRequest{GrantID: grant, Issue: "OTHER-5", Role: string(claim.RoleArchitect)}, nil},
			{"legion claims resume", route(tester, "resume"), nil, bearer},
			{"legion claims retry", route(architect, "retry"), nil, bearer},
			{"legion claims compact", route(architect, "compact"), nil, bearer},
			{"legion claims spawn", "/legion/v1/operator/claims", SpawnRequest{Tree: "OTHER-7", Issue: "OTHER-7", Role: claim.RoleArchitect, Task: "Plan it."}, bearer},
			{"legion claims deliver", route(architect, "deliver"), DeliverRequest{Task: "Read the state."}, bearer},
		} {
			failure := h.pauseRefused(request.target, request.body, request.header, http.StatusConflict, projectPaused)
			if failure.Pause == nil || failure.Pause.ID != began.ID {
				t.Fatalf("%s of a claim of another project's issue refused %+v, want it to name pause %d", request.name, failure, began.ID)
			}
		}
		after, err := h.store.Claims(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("the refused requests changed the claims:\nbefore %+v\nafter  %+v", before, after)
		}
		if got := len(h.runtime.Calls()); got != calls {
			t.Fatalf("the refused requests reached the runtime: %v", h.runtime.Methods()[calls:])
		}
	})

	// A daemon that cannot hold a pause answers PAUSE_UNAVAILABLE naming its setting, at both
	// doors, and one with no workflow, which has no pause service, answers it naming why.
	t.Run("a daemon that cannot hold a pause says which setting", func(t *testing.T) {
		offIdle := pauseCapable()
		offIdle.Runtime.Kubernetes.IdleRelease.IdleMinutes = 0
		offPacing := pauseCapable()
		offPacing.WorkerWakesPerMinute = 0
		for _, tc := range []struct {
			name    string
			cfg     config.Config
			setting pause.Setting
		}{
			{"runtime: tmux", config.Config{Runtime: config.Runtime{Name: "tmux"}, WorkerWakesPerMinute: 8}, pause.SettingTmux},
			{"idle_minutes: 0", offIdle, pause.SettingIdleReleaseOff},
			{"worker_wakes_per_minute: 0", offPacing, pause.SettingWakePacingOff},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := newPauseHarness(t, tc.cfg)
				grant := h.controllerSession()
				for _, refused := range []PauseFailure{
					h.pauseRefused(operatorPauseRoute, OperatorPauseRequest{Reason: "an outage"}, bearer, http.StatusConflict, pause.CodeUnavailable),
					h.pauseRefused(controllerPauseRoute, ProjectPauseRequest{GrantID: grant, Reason: "an outage"}, nil, http.StatusConflict, pause.CodeUnavailable),
				} {
					if refused.Setting != string(tc.setting) || !strings.Contains(refused.Error, string(tc.setting)) {
						t.Fatalf("the refusal %+v names %q, want the setting %q", refused, refused.Setting, tc.setting)
					}
				}
				if n := h.pauseCount(); n != 0 {
					t.Fatalf("a daemon that cannot hold a pause recorded %d", n)
				}
			})
		}
		t.Run("no workflow", func(t *testing.T) {
			h := newPauseHarness(t, pauseCapable())
			h.serveWith(nil)
			for target, body := range map[string]any{operatorPauseRoute: OperatorPauseRequest{Reason: "an outage"}, operatorResumeRoute: nil} {
				refused := h.pauseRefused(target, body, bearer, http.StatusConflict, pause.CodeUnavailable)
				if refused.Setting != "" || !strings.Contains(refused.Error, "no workflow") {
					t.Fatalf("a daemon with no workflow refused %+v, want the reason without a setting", refused)
				}
			}
		})
	})
}

// A caller from a pod below pauseContract is served the state without `paused` and `lastPause`,
// which its plugin's reader does not name; a pod at the contract, a pod whose claims record none
// and a caller in no pod are served the key as the source answered it. The paused state carries
// `paused` (pausedState); the last pause is the state of the record its fixture composes.
func TestTheStateRouteServesAPodItsPluginContractsPause(t *testing.T) {
	running := composedFixture(t, apifixture.Path("state-last-pause.json"))
	for _, state := range []struct {
		name  string
		state State
		key   string
	}{{"paused", pausedState(t), "paused"}, {"lastPause", running, "lastPause"}} {
		full := stateFrom(t, state.state, "192.0.2.1:41000", podClaims(pauseContract-1))
		if !strings.Contains(string(full), `"`+state.key+`"`) {
			t.Fatalf("the fixture's state carries no %s; this test proves nothing: %s", state.key, full)
		}
		for _, tc := range []struct {
			name   string
			remote string
			claims []supervise.Claim
			served bool
		}{
			{name: "a pod whose plugin is the contract before this one", remote: statePodAddr + ":41000", claims: podClaims(pauseContract - 1)},
			{name: "a pod below PodUpdateContract, below every narrowing", remote: statePodAddr + ":41000", claims: podClaims(supervise.PodUpdateContract - 1)},
			{name: "a pod at this contract", remote: statePodAddr + ":41000", claims: podClaims(pauseContract), served: true},
			{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: podClaims(0), served: true},
			{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(pauseContract - 1), served: true},
		} {
			t.Run(state.name+"/"+tc.name, func(t *testing.T) {
				body := stateFrom(t, state.state, tc.remote, tc.claims)
				var served map[string]json.RawMessage
				if err := json.Unmarshal(body, &served); err != nil {
					t.Fatal(err)
				}
				var want map[string]json.RawMessage
				if err := json.Unmarshal(full, &want); err != nil {
					t.Fatal(err)
				}
				got, carried := served[state.key]
				if carried != tc.served {
					t.Fatalf("the state carries %s = %v, want %v: %s", state.key, carried, tc.served, body)
				}
				if tc.served && string(got) != string(want[state.key]) {
					t.Fatalf("the state's %s = %s, want the source's %s", state.key, got, want[state.key])
				}
			})
		}
	}
}

// pauseContract is the contract whose entry in version.go added the two keys, and this daemon's
// own contract is never below it: a caller at this contract is served them.
func TestThePauseContractIsTheContractThatAddedTheKeys(t *testing.T) {
	const addedPause = 178
	if pauseContract != addedPause {
		t.Fatalf("pauseContract = %d, want %d, the contract whose entry in version.go added `paused` and `lastPause`",
			pauseContract, addedPause)
	}
	if pauseContract > DaemonAPIVersion() {
		t.Fatalf("pauseContract = %d, above DaemonAPIVersion %d: no caller could be served the keys",
			pauseContract, DaemonAPIVersion())
	}
}

// The state's read of the pause (PauseStateTx) over a database: nothing before any pause; while
// paused, the pause with the claims whose stop it waits on and the rows it holds; once it has
// ended, the last pause with who ended it and no `paused`.
func TestPauseStateTxReadsThePauseThroughTheStatesSnapshot(t *testing.T) {
	h := newPauseHarness(t, pauseCapable())
	ctx := context.Background()
	if err := h.store.PutClaim(ctx, supervise.Claim{Token: "legion-legion-legion-208-implementer", Project: testProject,
		Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer, Generation: 1, State: supervise.StateIdle}); err != nil {
		t.Fatal(err)
	}
	read := func() (*PausedView, *PauseView) {
		t.Helper()
		var paused *PausedView
		var last *PauseView
		if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			paused, last, err = PauseStateTx(ctx, tx, testDispatchProject, h.clock.Now())
			return err
		}); err != nil {
			t.Fatalf("read the pause state: %v", err)
		}
		return paused, last
	}
	if paused, last := read(); paused != nil || last != nil {
		t.Fatalf("before any pause the state reads %+v and %+v, want neither", paused, last)
	}

	began := beganPause(t, h.pauseOK(operatorPauseRoute, OperatorPauseRequest{Reason: "an outage"}, bearer))
	start, err := record.NewOutboxRow("LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208",
		Role: claim.RoleImplementer, Generation: 1}, pauseStartsAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error { return record.NewStore().Enqueue(ctx, tx, start) }); err != nil {
		t.Fatalf("queue a start the pause holds: %v", err)
	}
	h.clock.advance(2 * time.Minute)
	open, last := read()
	if open == nil || last != nil || open.ID != began.ID || open.Reason != "an outage" {
		t.Fatalf("while paused the state reads %+v and %+v, want pause %d and no last pause", open, last, began.ID)
	}
	if len(open.WaitsOn) != 1 || open.WaitsOn[0].Claim != "legion-legion-legion-208-implementer" || open.WaitsOn[0].Role != claim.RoleImplementer {
		t.Fatalf("the pause waits on %+v, want the implementer's stop", open.WaitsOn)
	}
	var startHeld bool
	for _, row := range open.Held {
		if row.Kind == string(record.OutboxKindSupervise) && row.Claim == "legion-legion-legion-208-implementer" && row.WaitedSeconds == 120 {
			startHeld = true
		}
	}
	if !startHeld {
		t.Fatalf("the pause holds %+v, want the implementer's start, due two minutes", open.Held)
	}

	h.pauseOK(operatorResumeRoute, nil, bearer)
	open, last = read()
	if open != nil || last == nil || last.ID != began.ID || last.EndedBy == nil || *last.EndedBy != (PauseActorView{Kind: "operator"}) {
		t.Fatalf("after the resume the state reads %+v and %+v, want pause %d as the last, ended by the operator", open, last, began.ID)
	}
}
