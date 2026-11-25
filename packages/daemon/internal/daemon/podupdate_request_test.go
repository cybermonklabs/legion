package daemon

// The update round the project's controller asks for over one issue's pod (requestPodUpdate,
// api's POST /legion/v1/issues/pod-update). Each test drives the daemon's own HTTP handler over a
// rig of podupdate_test.go or podupdate_fleet_test.go, with a controller grant minted as the
// grants route mints one, and then the rig's real sweep: what a test reads is what the route
// answered and what the store, the runtime and the outbox hold after it.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// podUpdateLogLine is the line the route writes for a request it answered with a round.
const podUpdateLogLine = "api: the controller asked for the replacement of an issue's pod"

// podUpdateRoute is the daemon's own HTTP handler serving the controller's pod update through a
// rig's supervision, as daemon.go wires it (api.Options.RequestPodUpdate), and the controller
// grant the requests carry.
type podUpdateRoute struct {
	t       *testing.T
	handler http.Handler
	grant   string
}

func newPodUpdateRoute(t *testing.T, s *supervision, sup *supervisor, log *slog.Logger) *podUpdateRoute {
	t.Helper()
	grants := credential.New(nil)
	// Generation 1 is the one a project's first controller registration takes
	// (store.MintController).
	grant, err := grants.MintController("ses_controller", 1)
	if err != nil {
		t.Fatalf("mint a controller grant: %v", err)
	}
	return &podUpdateRoute{t: t, grant: grant.ID, handler: api.NewServer("127.0.0.1", 0, api.Options{
		Supervisor: sup, Project: podUpdateProject, DispatchProject: "LEGION", Grants: grants, Log: log,
		RequestPodUpdate: s.requestPodUpdate,
	}).Handler}
}

// request is the controller's pod update of issue as the HTTP server hands it to the route, its
// context ctx: the one the request's connection lives in.
func (r *podUpdateRoute) request(ctx context.Context, issue string) *http.Request {
	r.t.Helper()
	body, err := json.Marshal(api.PodUpdateRequest{GrantID: r.grant, Issue: issue})
	if err != nil {
		r.t.Fatal(err)
	}
	return httptest.NewRequestWithContext(ctx, http.MethodPost, "/legion/v1/issues/pod-update", bytes.NewReader(body))
}

// ask posts the controller's pod update of issue, and answers the status and the body.
func (r *podUpdateRoute) ask(issue string) (int, []byte) {
	r.t.Helper()
	recorder := httptest.NewRecorder()
	r.handler.ServeHTTP(recorder, r.request(context.Background(), issue))
	return recorder.Code, recorder.Body.Bytes()
}

// answered asks for the pod update of issue and holds the answer to 200 and want.
func (r *podUpdateRoute) answered(issue string, want api.PodUpdateResponse) {
	r.t.Helper()
	status, body := r.ask(issue)
	if status != http.StatusOK {
		r.t.Fatalf("the pod update of %s = %d, want 200; body %s", issue, status, body)
	}
	var got api.PodUpdateResponse
	if err := json.Unmarshal(body, &got); err != nil {
		r.t.Fatalf("decode the answer %s: %v", body, err)
	}
	if got != want {
		r.t.Fatalf("the pod update of %s answered %+v, want %+v", issue, got, want)
	}
}

// refused asks for the pod update of issue, holds the answer to status and code, and answers its
// sentence.
func (r *podUpdateRoute) refused(issue string, status int, code string) string {
	r.t.Helper()
	got, body := r.ask(issue)
	var failure api.Failure
	if err := json.Unmarshal(body, &failure); err != nil {
		r.t.Fatalf("decode the refusal %s: %v", body, err)
	}
	if got != status || failure.Code != code {
		r.t.Fatalf("the pod update of %s = %d %s, want %d %s; body %s", issue, got, failure.Code, status, code, body)
	}
	return failure.Error
}

// requestedRound is what the route answers a request that opened the round over the rig's pod.
var requestedRound = api.PodUpdateResponse{Round: podUpdateRound, Reasons: requestedPodUpdateReasons, Opened: true}

// The controller's request opens the round a drift opens, over a pod that has drifted from
// nothing: each claim of the issue that has run a worker records the round with the reasons the
// controller's request gives, the claim with no worker records none, and the sweeps after it ask
// each ready role — the issue's architect among them — once, on its own role topic, with the
// round's one text, naming the round, its end at the bound, and those reasons. Nothing is replaced
// before the roles answer. The route logs one line naming the caller, the issue and the round.
//
// The negative control is the request's opening undone (requestPodUpdate posting nothing): no
// claim records the round, so the route refuses the request 503 POD_UPDATE_NOT_RECORDED where
// this test wants the round answered.
func TestPodUpdateTheControllerAsksForOpensTheRoundAndItsAskReachesEachReadyRole(t *testing.T) {
	asked := []claim.Role{claim.RoleArchitect, claim.RoleImplementer, claim.RoleTester}
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleArchitect),
		readyWorker(claim.RoleImplementer),
		readyWorker(claim.RoleTester),
		podUpdateRole{role: claim.RolePlanner, state: supervise.StateQueued},
	)
	route := newPodUpdateRoute(t, rig.s, rig.sup, rig.log)

	route.answered(podUpdateIssue, requestedRound)

	for _, role := range asked {
		if c := rig.awaitRound(role, podUpdateRound); c.UpdateReasons != requestedPodUpdateReasons {
			t.Errorf("the %s records the reasons %q, want %q", role, c.UpdateReasons, requestedPodUpdateReasons)
		}
	}
	if c := rig.claimOf(claim.RolePlanner); c.UpdateRound != "" {
		t.Errorf("the planner, which never had a worker, is in round %q; want none", c.UpdateRound)
	}
	for _, role := range asked {
		rig.awaitAsked(role, podUpdateRound)
	}
	rows := map[claim.Role]int{}
	for _, ask := range rig.asks() {
		rows[ask.Role]++
		// The text names what the role's push keeps, so each role's is its own (main's #5161).
		text := supervise.UpdateTask(ask.Role, podUpdateRound, requestedPodUpdateReasons, podUpdateStart.Add(podUpdateGrace))
		if ask.Round != podUpdateRound || ask.Pod != podUpdatePod || ask.Text != text {
			t.Errorf("the ask to the %s = %+v, want round %s of pod %s with the text:\n%s", ask.Role, ask, podUpdateRound, podUpdatePod, text)
		}
	}
	_, published := rig.listener.snapshot()
	topics := map[string]int{}
	for _, publish := range published {
		topics[publish.topic]++
	}
	for _, role := range asked {
		if rows[role] != 1 || topics[rig.roleTopic(role)] != 1 {
			t.Errorf("the %s was asked %d times and its topic delivered %d asks, want one of each", role, rows[role], topics[rig.roleTopic(role)])
		}
	}
	if len(rows) != len(asked) {
		t.Errorf("asks went to %v, want the %v alone", rows, asked)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times before any role answered", len(calls))
	}
	lines := loggedLines(t, rig.logs.String(), `"msg":"`+podUpdateLogLine+`"`)
	if len(lines) != 1 || lines[0]["issue"] != podUpdateIssue || lines[0]["round"] != podUpdateRound || lines[0]["opened"] != true {
		t.Fatalf("lines %q = %v, want one naming %s and round %s opened", podUpdateLogLine, lines, podUpdateIssue, podUpdateRound)
	}
}

// A round the controller asked for over a pod that has drifted from nothing is carried by the
// sweeps that follow to the replacement of its pod, as a drift's round is: a round open over the
// pod its claims record goes on, drift or no drift (reconcileIssuePodUpdate). Once every role it
// asks has answered, the next sweep replaces the pod — once, the process of the round's own pod
// named — and ends the round on every claim.
//
// The negative control is the sweep reading an open round over a pod that has not drifted as
// nothing to do (the `len(reasons) == 0 && open == ""` early return without its `open` half): no
// sweep asks a role, and the wait for the first ask times out.
func TestPodUpdateTheControllerAsksForIsCarriedToThePodsReplacementWithNoDrift(t *testing.T) {
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	route := newPodUpdateRoute(t, rig.s, rig.sup, quietLogger())

	route.answered(podUpdateIssue, requestedRound)
	for _, role := range roles {
		rig.awaitAsked(role, podUpdateRound)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times before any role answered", len(calls))
	}
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.tick()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times while the tester had not answered", len(calls))
	}
	rig.signal(claim.RoleTester, "")

	calls := rig.awaitReplacement(1, "once every role answered the round the controller asked for over a pod that never drifted")
	if calls[0].Pod.Locator == nil || calls[0].Pod.Locator.Sandbox.PodUID != podUpdatePod {
		t.Fatalf("ReplacePod called with %+v, want a process of pod %s", calls[0].Pod, podUpdatePod)
	}
	for _, role := range roles {
		rig.awaitClaim(role, "its round to end with the replacement", func(c supervise.Claim) bool {
			return c.UpdateRound == "" && c.UpdateSignalledAt == nil
		})
	}
}

// charged is this daemon's own record of its openings (podUpdateOpenings' fields, read without
// dropping any): when it last opened a round, and each opening no claim has recorded yet.
func charged(s *supervision) (time.Time, map[string]podUpdateOpening) {
	s.podUpdateMu.Lock()
	defer s.podUpdateMu.Unlock()
	return s.podUpdateOpenedAt, maps.Clone(s.podUpdateOpened)
}

// A request over a pod a round is open over already opens nothing: it answers that round, with
// the reasons the round records and opened false, and charges nothing to the project's limits.
// That is so for a drift's round the claims record, for an opening of this daemon's own whose post
// no claim has recorded yet, and for the controller's own earlier request.
//
// The negative control is the request opening whatever it finds (no answer of a round open or in
// flight): it answers opened true, with the controller's reasons, in each case, and charges the
// opening it makes.
func TestPodUpdateTheControllerAsksForOverAnOpenRoundOpensNoSecond(t *testing.T) {
	t.Run("a drift's round the claims record", func(t *testing.T) {
		rig := newPodUpdateRig(t,
			podUpdateRole{role: claim.RoleImplementer, state: supervise.StateReady, session: true, inPod: true,
				contract: api.DaemonAPIVersion(), round: podUpdateRound, reasons: podUpdateDrift},
			podUpdateRole{role: claim.RoleTester, state: supervise.StateReady, session: true, inPod: true,
				contract: api.DaemonAPIVersion(), round: podUpdateRound, reasons: podUpdateDrift},
		)
		route := newPodUpdateRoute(t, rig.s, rig.sup, quietLogger())

		route.answered(podUpdateIssue, api.PodUpdateResponse{Round: podUpdateRound, Reasons: podUpdateDrift})

		if opened, inFlight := charged(rig.s); !opened.IsZero() || len(inFlight) != 0 {
			t.Fatalf("the request charged an opening at %v, in flight %v, want nothing charged", opened, inFlight)
		}
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
			rig.settle(role)
			if c := rig.claimOf(role); c.UpdateReasons != podUpdateDrift || !c.UpdateOpenedAt.Equal(podUpdateStart) {
				t.Errorf("the %s records reasons %q opened at %v, want the drift's round as it stood: %q at %s",
					role, c.UpdateReasons, c.UpdateOpenedAt, podUpdateDrift, podUpdateStart)
			}
		}
	})
	t.Run("an opening still in flight", func(t *testing.T) {
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
		route := newPodUpdateRoute(t, rig.s, rig.sup, quietLogger())
		// A sweep's opening whose post no machine has taken yet, which is what this daemon's
		// record holds between the post and a claim's record of it.
		rig.s.recordPodUpdateOpening(&podUpdateSweep{}, podUpdateIssue, podUpdateRound, podUpdateDrift, podUpdateStart)
		rig.clock.advance(time.Minute)

		route.answered(podUpdateIssue, api.PodUpdateResponse{Round: podUpdateRound, Reasons: podUpdateDrift})

		if opened, inFlight := charged(rig.s); !opened.Equal(podUpdateStart) || len(inFlight) != 1 {
			t.Fatalf("this daemon's record holds an opening at %v, in flight %v, want the sweep's one at %s alone", opened, inFlight, podUpdateStart)
		}
	})
	t.Run("the controller's own earlier request", func(t *testing.T) {
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
		route := newPodUpdateRoute(t, rig.s, rig.sup, quietLogger())

		route.answered(podUpdateIssue, requestedRound)
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
			rig.awaitRound(role, podUpdateRound)
		}
		rig.clock.advance(time.Minute)
		route.answered(podUpdateIssue, api.PodUpdateResponse{Round: podUpdateRound, Reasons: requestedPodUpdateReasons})

		if opened, _ := charged(rig.s); !opened.Equal(podUpdateStart) {
			t.Fatalf("this daemon's last opening is at %v, want the first request's %s: the second charged one", opened, podUpdateStart)
		}
	})
}

// The project's limits on openings hold back the controller's request as they hold back a
// drift's opening at a sweep, and the refusal names the limit: with pod_update_max_open_rounds at
// one and another issue's round open, the request is refused naming that limit; with
// pod_update_rounds_per_minute at one and a round opened less than a minute ago, naming that one.
// Nothing is opened either time. A minute later the same request opens the round.
//
// The negative control is the request opening past the limits (no podUpdateSweep.holds): the
// first request opens the round over the second issue's pod while the first issue holds the one
// slot.
func TestPodUpdateTheControllerAsksForIsHeldByTheProjectsLimits(t *testing.T) {
	drifted, asked := "LEGION-501", "LEGION-502"
	rig := newLimitRig(t, drifted, asked)
	rig.drifts(asked)
	rig.s.cfg.PodUpdateMaxOpenRounds = new(1)
	route := newPodUpdateRoute(t, rig.s, rig.sup, quietLogger())

	rig.tick()
	rig.awaitRounds("the sweep opened the drifted issue's round", drifted)

	if refusal := route.refused(asked, http.StatusConflict, "POD_UPDATE_LIMITED"); !strings.Contains(refusal, "pod_update_max_open_rounds (1)") {
		t.Errorf("the refusal under the cap on open rounds says %q, want it to name pod_update_max_open_rounds (1)", refusal)
	}
	rig.s.cfg.PodUpdateMaxOpenRounds = nil
	rig.s.cfg.PodUpdateRoundsPerMinute = new(1)
	if refusal := route.refused(asked, http.StatusConflict, "POD_UPDATE_LIMITED"); !strings.Contains(refusal, "pod_update_rounds_per_minute (1)") {
		t.Errorf("the refusal under the rounds a minute says %q, want it to name pod_update_rounds_per_minute (1)", refusal)
	}
	rig.awaitRounds("two refused requests", drifted)

	rig.clock.advance(time.Minute)
	route.answered(asked, api.PodUpdateResponse{Round: rig.round(asked), Reasons: requestedPodUpdateReasons, Opened: true})
	rig.awaitRounds("the request a minute later", drifted, asked)
}

// A request the daemon can open no round for is refused, and opens nothing: an issue no claim of
// this daemon is on (404 NO_CLAIM), an issue whose claims record no pod — each suspended, which
// clears the locator — and one whose claims run no worker in their pod — none has a session yet —
// (404 NO_POD), and an issue whose pod is changing (409 POD_CHANGING): its claims record two pods,
// a replacement in flight, or a round over the pod before this one, which the next sweep ends. Each
// refusal's sentence names the reading that refused it.
//
// The negative control is each refusal undone, the request going on to the opening: the round is
// posted over a pod named by no claim, over a pod no round can reach, over one of two pods, or over
// the record of the round that is ending.
func TestPodUpdateTheControllerAsksForIsRefusedWhereNoRoundCanOpen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		roles  []podUpdateRole
		status int
		code   string
		// says is the part of the refusal's sentence that names the reading which refused it.
		says string
	}{
		{name: "no claim records a pod", status: http.StatusNotFound, code: "NO_POD", says: "no claim of " + podUpdateIssue + " records a process in a pod",
			roles: []podUpdateRole{
				{role: claim.RoleImplementer, state: supervise.StateSuspended, session: true, contract: api.DaemonAPIVersion(),
					suspendedBy: supervise.SuspendedByOperator},
			}},
		{name: "no claim runs a worker in the pod", status: http.StatusNotFound, code: "NO_POD", says: "no claim of " + podUpdateIssue + " runs a worker in its pod " + podUpdatePod,
			roles: []podUpdateRole{
				{role: claim.RoleImplementer, state: supervise.StateLaunching, inPod: true},
			}},
		{name: "the claims record two pods", status: http.StatusConflict, code: "POD_CHANGING", says: "the claims of " + podUpdateIssue + " record two pods",
			roles: []podUpdateRole{
				readyWorker(claim.RoleImplementer),
				{role: claim.RoleTester, state: supervise.StateReady, session: true, contract: api.DaemonAPIVersion(), pod: podUpdateReplacementPod},
			}},
		{name: "a round over the pod before this one is ending", status: http.StatusConflict, code: "POD_CHANGING", says: "is over a pod its claims no longer record",
			roles: []podUpdateRole{
				{role: claim.RoleImplementer, state: supervise.StateReady, session: true, inPod: true, contract: api.DaemonAPIVersion(),
					round: supervise.PodRound(podUpdatePreviousPod), reasons: podUpdateDrift},
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newPodUpdateRig(t, tc.roles...)
			route := newPodUpdateRoute(t, rig.s, rig.sup, quietLogger())

			if refusal := route.refused(podUpdateIssue, tc.status, tc.code); !strings.Contains(refusal, tc.says) {
				t.Errorf("the refusal says %q, want it to say %q", refusal, tc.says)
			}
			route.refused("LEGION-999", http.StatusNotFound, "NO_CLAIM")

			for _, role := range tc.roles {
				rig.settle(role.role)
				if c := rig.claimOf(role.role); c.UpdateRound != role.round {
					t.Errorf("the %s records round %q after a refused request, want %q as it was seeded", role.role, c.UpdateRound, role.round)
				}
			}
			if opened, inFlight := charged(rig.s); !opened.IsZero() || len(inFlight) != 0 {
				t.Fatalf("the refused request charged an opening at %v, in flight %v, want nothing charged", opened, inFlight)
			}
		})
	}
}

// podUpdatePostedLine is the line the daemon writes for a round the controller's request opened
// (postRequestedPodUpdate). Its roles field names the claims whose machines record the round
// (noticeClaims): each claim the round is posted to that has not failed.
const podUpdatePostedLine = "supervise: the project's controller asked for the replacement of this issue's pod; its update round is posted"

// A claim that records no process and whose launch waits on the issue's pod for that pod's round
// (supervise.Claim.AwaitsPod) is a claim the controller's request counts, with a session or with
// none: the round is posted to it (postedClaims), and its machine records the round
// (noticeClaims). Such a claim may be the issue's only one, and the round is its one way to the
// pod. Here the issue's only claim is an implementer whose first launch the runtime refused for
// want of the pod's round, with no session and no process after it (refusedFirstLaunch). The
// request is not refused 404 NO_POD: it opens the round over the pod that launch waits on, the
// implementer records the round with the controller's reasons, and the daemon's line names the
// implementer as the one role the round is posted to.
//
// The negative control is the request counting the claims with a session alone (sessionClaims in
// place of noticeClaims): it is refused 404 NO_POD where this test wants the round opened. Where
// the daemon's line alone names the claims with a session (namesOf(sessionClaims(told))), it
// names no role where this test wants the implementer.
func TestPodUpdateTheControllerAsksForOpensTheRoundOverThePodItsOnlyClaimsLaunchWaitsOn(t *testing.T) {
	rig := newPodUpdateRig(t, podUpdateRole{role: claim.RoleImplementer, state: supervise.StateQueued})
	refusals := rig.refuseLaunches()
	refusals.refuse(claim.RoleImplementer, podUpdatePod)
	rig.refusedFirstLaunch(claim.RoleImplementer)
	route := newPodUpdateRoute(t, rig.s, rig.sup, quietLogger())

	route.answered(podUpdateIssue, requestedRound)

	if c := rig.awaitRound(claim.RoleImplementer, podUpdateRound); c.UpdateReasons != requestedPodUpdateReasons {
		t.Errorf("the implementer records the reasons %q, want %q", c.UpdateReasons, requestedPodUpdateReasons)
	}
	lines := loggedLines(t, rig.logs.String(), `"msg":"`+podUpdatePostedLine+`"`)
	if len(lines) != 1 || lines[0]["issue"] != podUpdateIssue || lines[0]["round"] != podUpdateRound {
		t.Fatalf("lines %q = %v, want one naming %s and round %s", podUpdatePostedLine, lines, podUpdateIssue, podUpdateRound)
	}
	if roles, _ := lines[0]["roles"].([]any); len(roles) != 1 || roles[0] != string(claim.RoleImplementer) {
		t.Errorf("the line %q names the roles %v, want the %s alone, the one claim the round is posted to",
			podUpdatePostedLine, lines[0]["roles"], claim.RoleImplementer)
	}
}

// A round this daemon ended is no round for the controller's request to answer or to open over
// again while a claim of the issue still records it (podUpdateEnded).
// The end is posted to each member's machine, and a machine in a relaunch takes it only once the
// relaunch is done, so its claim keeps the round until then. The request reads the issue as that
// end leaves it, as the sweep does (podUpdateSweep.asEnded): where no other round is left, it is
// refused 409 POD_CHANGING, naming the ended round, and it posts nothing, opens nothing and charges
// nothing. Once that machine has taken the end, the same request opens a new round over the pod
// the claims record.
//
// Here the round's pod goes for a failure of its own while the implementer's machine relaunches it
// into the pod in its place (holdRelaunch), as TestARoundOverAGonePodThatLostWorkWritesOnce has it:
// the sweep ends the round, the tester takes the end, and the implementer's claim keeps the round
// behind its relaunch.
//
// The negative control is the request reading the group as it stands (no asEnded): the
// implementer's record of the ended round is the round open over the pod the tester records, and
// the request answers that round 200 where this test wants it refused.
func TestPodUpdateTheControllerAsksForIsRefusedWhileARoundThisDaemonEndedIsStillRecorded(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	route := newPodUpdateRoute(t, rig.s, rig.sup, quietLogger())
	rig.drifts(podUpdateDrift)
	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.settleAll()

	release := rig.holdRelaunch(claim.RoleImplementer, false, podUpdateReplacementPod)
	rig.podGone()
	rig.awaitSwept(claim.RoleTester, "the end of the round over the gone pod to reach the tester",
		func(c supervise.Claim) bool { return c.UpdateRound == "" })
	if round := rig.storedRound(claim.RoleImplementer); round != podUpdateRound {
		t.Fatalf("the implementer's claim records round %q, want %s: its machine is relaunching and has not taken the end", round, podUpdateRound)
	}
	rig.s.podUpdateEndedMu.Lock()
	ended := rig.s.podUpdateEnded[podUpdateIssue]
	rig.s.podUpdateEndedMu.Unlock()
	if ended != podUpdateRound {
		t.Fatalf("this daemon keeps %q as the ended round of %s, want %s", ended, podUpdateIssue, podUpdateRound)
	}
	openedBefore, _ := charged(rig.s)

	refusal := route.refused(podUpdateIssue, http.StatusConflict, "POD_CHANGING")
	if want := "the update round " + podUpdateRound + " of " + podUpdateIssue + " has ended"; !strings.Contains(refusal, want) {
		t.Errorf("the refusal says %q, want it to say %q", refusal, want)
	}
	if opened, inFlight := charged(rig.s); !opened.Equal(openedBefore) || len(inFlight) != 0 {
		t.Fatalf("the refused request charged an opening at %v, in flight %v, want this daemon's last opening at %v and nothing in flight",
			opened, inFlight, openedBefore)
	}
	rig.awaitPosted(claim.RoleTester)
	if c := rig.claimOf(claim.RoleTester); c.UpdateRound != "" {
		t.Fatalf("the tester records round %q after the refused request, want none: the request posted nothing", c.UpdateRound)
	}

	// The implementer's relaunch comes up in the pod in the gone pod's place, and its machine
	// takes the end queued behind it, with nothing of the refused request after the end. The
	// tester's process ends with the gone pod too, and comes back beside it.
	release()
	rig.awaitPosted(claim.RoleImplementer)
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != "" {
		t.Fatalf("the implementer records round %q once its machine took what was posted to it, want none", c.UpdateRound)
	}
	rig.displacedInto(claim.RoleTester, podUpdateReplacementPod)

	next := supervise.PodRound(podUpdateReplacementPod)
	route.answered(podUpdateIssue, api.PodUpdateResponse{Round: next, Reasons: requestedPodUpdateReasons, Opened: true})
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		if c := rig.awaitRound(role, next); c.UpdateReasons != requestedPodUpdateReasons {
			t.Errorf("the %s records the reasons %q, want %q", role, c.UpdateReasons, requestedPodUpdateReasons)
		}
	}
}

// The controller's request reads the rounds this daemon ended and its openings still in flight
// without pruning either record: only the sweep's walk prunes them (readPodUpdateSweep,
// beginPodUpdateSweep). The walk reads its claims before it takes the turn the request holds
// (reconcileOrphans), so a request can take the turn in between, over claims read after the
// walk's. Here a request comes in that window after the relaunching member's machine has taken the
// end of a round this daemon ended, and the walk then decides from its older read, on which that
// member's claim still records the round. The issue is told once that its pod went with that
// member's work (review 6006134246 on cybermonklabs/legion#4065).
//
// The round's pod goes while the implementer, which had not answered, relaunches behind a held
// Resume (holdRelaunch), as in TestARoundOverAGonePodThatLostWorkWritesOnce, and the tester, which
// had answered, has come back in the pod in the gone pod's place. The sweep reads the claims
// recording that pod, ends the round, writes one message, and keeps the round as ended
// (podUpdateEnded) while the implementer's claim records it. The walk's claims are read then. The
// implementer's relaunch comes up and its machine takes the end, the controller's request opens
// the round over the pod in the gone pod's place, and the walk runs over its older read.
//
// The negative control is the request reading through the walk's pruning read
// (beginPodUpdateSweep in postRequestedPodUpdate): the request drops the ended round, which no
// claim it reads records, and the walk reads the round open over a pod its claims no longer
// record, ends it again (endRoundWhosePodWent) and writes a second message on the issue.
func TestPodUpdateTheControllerAsksForBetweenAWalksReadAndItsTurnWritesNoSecondLostWorkMessage(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	route := newPodUpdateRoute(t, rig.s, rig.sup, quietLogger())
	rig.drifts(podUpdateDrift)
	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.settleAll()

	// The tester's relaunch is made whole before the implementer's is held: the fake runtime
	// answers its Resumes in the order they were scripted, and a held Resume takes its answer only
	// once it is let go.
	rig.displacedInto(claim.RoleTester, podUpdateReplacementPod)
	release := rig.holdRelaunch(claim.RoleImplementer, false, podUpdateReplacementPod)
	rig.tick()
	messages := rig.messages()
	if len(messages) != 1 {
		t.Fatalf("the sweep that ended the round wrote on the issue %d times, want once for the pod that went: %q", len(messages), messages)
	}
	if !strings.Contains(messages[0], string(claim.RoleImplementer)) || strings.Contains(messages[0], string(claim.RoleTester)) {
		t.Errorf("the message does not name the implementer alone, the one member that had not answered: %s", messages[0])
	}
	rig.awaitClaim(claim.RoleTester, "the end of the round to reach the tester",
		func(c supervise.Claim) bool { return c.UpdateRound == "" })

	// The walk's read of the claims, made before it takes the turn.
	snapshot := rig.snapshot()
	recorded := ""
	for _, c := range snapshot {
		if c.Token == rig.tokens[claim.RoleImplementer] {
			recorded = c.UpdateRound
		}
	}
	if recorded != podUpdateRound {
		t.Fatalf("the implementer's claim records round %q in the walk's read, want %s: its machine is relaunching and has not taken the end", recorded, podUpdateRound)
	}

	// The implementer's relaunch comes up beside the tester and its machine takes the end. The
	// controller's request then takes the turn ahead of the walk, over claims that record the
	// ended round on no claim, and opens the round over the pod in the gone pod's place.
	release()
	rig.awaitPosted(claim.RoleImplementer)
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != "" {
		t.Fatalf("the implementer records round %q once its machine took what was posted to it, want none", c.UpdateRound)
	}
	next := supervise.PodRound(podUpdateReplacementPod)
	route.answered(podUpdateIssue, api.PodUpdateResponse{Round: next, Reasons: requestedPodUpdateReasons, Opened: true})

	// The walk takes the turn after the request, and decides from its own older read.
	rig.s.reconcilePodUpdates(rig.ctx, snapshot)

	if messages := rig.messages(); len(messages) != 1 {
		t.Fatalf("the issue was written on %d times, want once for the pod that went: the walk after the request ended again the round this daemon had ended: %q", len(messages), messages)
	}
}

// storedRound is the round each claim of issue records in the store, by role: what a daemon that
// boots now restores.
func (r *fleetRig) storedRound(issue string) map[claim.Role]string {
	r.t.Helper()
	claims, err := r.sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the stored claims: %v", err)
	}
	rounds := map[claim.Role]string{}
	for _, c := range claims {
		if c.Issue == issue {
			rounds[c.Role] = c.UpdateRound
		}
	}
	return rounds
}

// A 200 is a round a claim of the issue records in the store, and nothing less. The issue's one
// worker the round asks, its architect, is held inside a Resume, as a relaunch into its pod holds
// it, so the round's post waits in its machine's inbox and no claim can record it: the request is
// refused 503 POD_UPDATE_NOT_RECORDED at its bound, and the store holds no round. Answered 200 at
// the post instead, the controller would read the round as open, and then — once a restart had
// dropped the post with this daemon's record of the opening, or the claim had failed under it —
// read the claims recording no round as the round done, over a pod nothing replaced. Once the
// relaunch ends the machine takes the post, and the next request answers the round its claim
// records.
//
// The negative control is the answer made at the post (no awaitPodRoundRecorded): the request
// answers 200 opened true while no claim records the round.
func TestPodUpdateTheControllerAsksForIsAnsweredOnlyOnceAClaimRecordsItsRound(t *testing.T) {
	issue := "LEGION-503"
	held := limitIssue(1, issue)
	held.roles = []claim.Role{claim.RoleArchitect, claim.RoleImplementer}
	held.unregistered = []claim.Role{claim.RoleImplementer}
	rig := newFleetRig(t, held)
	rig.s.podRoundRecordBound = 300 * time.Millisecond
	route := newPodUpdateRoute(t, rig.s, rig.sup, quietLogger())

	architect := rig.token(issue, claim.RoleArchitect)
	died := *rig.claimOf(issue, claim.RoleArchitect).Locator
	rig.rt.ScriptResume(fake.SpawnResult{Locator: *podUpdateLocator(architect, issue, rig.pods[issue])})
	entered, release := make(chan struct{}), make(chan struct{})
	releaseMachine := sync.OnceFunc(func() { close(release) })
	rig.rt.HoldResume(func() {
		rig.rt.HoldResume(nil)
		close(entered)
		<-release
	})
	defer releaseMachine()
	go func() {
		m, _ := rig.sup.Machine(architect)
		_ = m.Handle(rig.ctx, supervise.RuntimeObservation{
			Observation: runtime.Observation{Locator: died, Kind: runtime.Gone, Detail: "the process ended"},
		})
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the architect's machine never reached the Resume this test holds it in")
	}

	if refusal := route.refused(issue, http.StatusServiceUnavailable, "POD_UPDATE_NOT_RECORDED"); !strings.Contains(refusal, "no claim recorded it") {
		t.Errorf("the refusal says %q, want it to say no claim recorded the round", refusal)
	}
	for role, round := range rig.storedRound(issue) {
		if round != "" {
			t.Fatalf("the %s's stored claim records round %q after a refused request", role, round)
		}
	}

	releaseMachine()
	rig.awaitRound(issue, claim.RoleArchitect)
	route.answered(issue, api.PodUpdateResponse{Round: rig.round(issue), Reasons: requestedPodUpdateReasons})
}

// A round the controller asked for, over a pod that never drifted, is in the store when the
// request answers 200, so a daemon that boots straight after the answer — with none of the first
// daemon's memory, its record of the opening included, and a runtime that reads no drift — carries
// it: its sweeps take the worker's answer and replace the pod, and the round ends.
func TestPodUpdateTheControllerAsksForSurvivesARestartAndIsCarriedWithNoDrift(t *testing.T) {
	issue := "LEGION-504"
	rig := newFleetRig(t, limitIssue(1, issue))
	route := newPodUpdateRoute(t, rig.s, rig.sup, quietLogger())

	route.answered(issue, api.PodUpdateResponse{Round: rig.round(issue), Reasons: requestedPodUpdateReasons, Opened: true})
	if stored := rig.storedRound(issue)[claim.RoleArchitect]; stored != rig.round(issue) {
		t.Fatalf("the architect's stored claim records round %q when the request answered, want %s", stored, rig.round(issue))
	}

	next := rig.restart()
	if opened, inFlight := charged(next.s); !opened.IsZero() || len(inFlight) != 0 {
		t.Fatalf("the restarted daemon holds an opening at %v, in flight %v, want none of the first one's memory", opened, inFlight)
	}
	next.awaitRound(issue, claim.RoleArchitect)
	next.answer(issue)
	next.sweepUntilReplaced(issue)
	next.awaitRoundEnded(issue)
	next.replacedOnce(issue)
}

// A request that comes while a sweep's walk over the project's issues holds the walk's turn
// (podUpdateWalk) waits for that turn while the request lives and at most the bound of its wait
// for the round's record (roundRecordBound): the API sets no deadline on a request and the
// controller's client sets no timeout, so a caller still there would otherwise wait out the whole
// walk. The test holds the turn as a walk holds it (walkPodUpdates). A request whose context ends
// first, under the default bound, waits while its context lives and is then refused 503
// POD_UPDATE_BUSY, its sentence saying the request ended; a request whose context never ends is
// refused the same once the rig's short bound passes, its sentence naming that bound. Each
// carries a Retry-After of the sweep's interval in seconds, and each read nothing and opened
// nothing: no claim records a round once its machine has handled every event posted to it,
// nothing is charged to the project's limits, and no ask is written. Once the walk gives the turn
// back, the same request opens the round.
//
// The negative control is the request's wait for the turn bounded by its context alone (the
// select on ctx.Done(), with no bound): the request whose context never ends waits for the turn
// past the bound, and no answer comes.
func TestPodUpdateTheControllerAsksForWaitsForASweepsWalkWhileItsRequestLivesAndAtMostItsBound(t *testing.T) {
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	route := newPodUpdateRoute(t, rig.s, rig.sup, quietLogger())

	walk := rig.s.podUpdateWalkTurn()
	walk <- struct{}{}
	// serve hands the route the request ctx lives in, and answers its answer once it comes.
	serve := func(ctx context.Context) <-chan *httptest.ResponseRecorder {
		request := route.request(ctx, podUpdateIssue)
		answers := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			recorder := httptest.NewRecorder()
			route.handler.ServeHTTP(recorder, request)
			answers <- recorder
		}()
		return answers
	}
	// busy holds an answer to the sweep's busy refusal and answers its sentence.
	busy := func(recorder *httptest.ResponseRecorder) string {
		t.Helper()
		var failure api.Failure
		if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
			t.Fatalf("decode the refusal %s: %v", recorder.Body, err)
		}
		if recorder.Code != http.StatusServiceUnavailable || failure.Code != "POD_UPDATE_BUSY" {
			t.Fatalf("the request = %d %s, want 503 POD_UPDATE_BUSY; body %s", recorder.Code, failure.Code, recorder.Body)
		}
		if got, want := recorder.Header().Get("Retry-After"), strconv.Itoa(int(orphanSweepInterval/time.Second)); got != want {
			t.Errorf("Retry-After = %q, want %q, the sweep's interval in seconds", got, want)
		}
		return failure.Error
	}

	// A request whose context ends before the default bound passes.
	ctx, end := context.WithCancel(context.Background())
	defer end()
	answers := serve(ctx)
	select {
	case recorder := <-answers:
		t.Fatalf("the request answered %d while its context lived and the walk held the turn; body %s", recorder.Code, recorder.Body)
	case <-time.After(200 * time.Millisecond):
	}
	end()
	var recorder *httptest.ResponseRecorder
	select {
	case recorder = <-answers:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never answered once its context ended, while the walk held the turn")
	}
	if refusal := busy(recorder); !strings.Contains(refusal, "the request ended (context canceled)") || !strings.Contains(refusal, defaultPodRoundRecordBound.String()) {
		t.Errorf("the refusal says %q, want it to say the request ended, and to name the bound %s", refusal, defaultPodRoundRecordBound)
	}

	// A request whose context never ends, once the rig's short bound passes.
	rig.s.podRoundRecordBound = 300 * time.Millisecond
	asked := time.Now()
	answers = serve(context.Background())
	select {
	case recorder = <-answers:
	case <-time.After(10 * time.Second):
		t.Fatalf("a live request never answered within %s while the walk held the turn, though its bound is %s", 10*time.Second, rig.s.podRoundRecordBound)
	}
	if waited := time.Since(asked); waited < rig.s.podRoundRecordBound {
		t.Errorf("the live request answered after %s, want it to wait out its bound of %s", waited, rig.s.podRoundRecordBound)
	}
	if refusal := busy(recorder); !strings.Contains(refusal, "the request's wait for it passed its bound") || !strings.Contains(refusal, rig.s.podRoundRecordBound.String()) {
		t.Errorf("the refusal says %q, want it to say the request's wait passed its bound, and to name the bound %s", refusal, rig.s.podRoundRecordBound)
	}

	for _, role := range roles {
		handled := make(chan struct{})
		rig.sup.drained(rig.tokens[role], handled)
		<-handled
		if c := rig.claimOf(role); c.UpdateRound != "" {
			t.Errorf("the %s records round %q after the refused requests, want none", role, c.UpdateRound)
		}
	}
	if opened, inFlight := charged(rig.s); !opened.IsZero() || len(inFlight) != 0 {
		t.Errorf("the refused requests charged an opening at %v, in flight %v, want nothing charged", opened, inFlight)
	}
	if asks := rig.asks(); len(asks) != 0 {
		t.Errorf("the refused requests left %d asks, want none: %+v", len(asks), asks)
	}

	<-walk
	route.answered(podUpdateIssue, requestedRound)
	for _, role := range roles {
		rig.awaitRound(role, podUpdateRound)
	}
}
