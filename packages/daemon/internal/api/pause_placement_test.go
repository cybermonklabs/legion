package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// These tests drive three writers of a body placedByPod names through the server's handler: the
// release-records route, a pause refused PAUSE_UNAVAILABLE for the daemon's configuration, and a
// compaction refused PROJECT_PAUSED (refusedWhilePaused). Each is asked as three callers: a pod
// whose plugin is the contract before pauseContract, a pod at it, and a caller in no pod that
// states this daemon's contract. The other writers of a PauseFailure — PROJECT_ALREADY_PAUSED,
// the resume's PROJECT_NOT_PAUSED, refuseWithoutPause on the pause and the resume,
// refusedWhilePaused on its other requests, and startProjectRelease's PROJECT_PAUSED — are placed
// because they write through writeAnswer, which reads placedByPod; no route test here drives them.

const (
	// pauseAtPodUID is a second pod of the project, whose plugin records pauseContract, and
	// pauseAtPodAddr its address. The pod at statePodAddr, statePodUID, is the one below it.
	pauseAtPodUID  = "c3a9e2d4-5b1f-4e8a-9d70-6f2b8e4a1c95"
	pauseAtPodAddr = "10.1.24.7"
)

// pausePodClaims is the project's claims as the daemon's Supervisor.Claims answers them: the pod
// at statePodAddr, whose plugin records the contract before pauseContract (podClaims, with its
// claim of a third pod at this daemon's contract), and the pod at pauseAtPodAddr, whose plugin
// records pauseContract.
func pausePodClaims() []supervise.Claim {
	return append(podClaims(pauseContract-1),
		claimInPod("legion-legion-legion-211-planner", pauseAtPodUID, pauseContract),
		claimInPod("legion-legion-legion-211-implementer", pauseAtPodUID, pauseContract))
}

// placePodCallers replaces the pause harness's server with one that places its callers by pod, as
// a daemon of the Sandbox runtime does (Options.CallerPod): statePodAddr is in statePodUID,
// pauseAtPodAddr in pauseAtPodUID, and every other address in no pod. The project's claims are
// pausePodClaims (podSupervisor); the machines stay the harness's own. The rest is serveWith's
// daemon over the harness's pause service (pauseOptions), which keeps the release records too
// (Options.ReleaseRecords). The grants are this server's own, so a test mints its grants after it.
func (h *pauseHarness) placePodCallers() {
	h.t.Helper()
	pods := map[netip.Addr]string{
		netip.MustParseAddr(statePodAddr):   statePodUID,
		netip.MustParseAddr(pauseAtPodAddr): pauseAtPodUID,
	}
	options := h.pauseOptions(h.svc)
	options.Supervisor = podSupervisor{testSupervisor: h.supervisor, claims: pausePodClaims()}
	options.ReleaseRecords = h.store
	options.CallerPod = func(addr netip.Addr) (string, bool) {
		pod, inPod := pods[addr]
		return pod, inPod
	}
	h.handler = NewServer("127.0.0.1", 8437, options).Handler
}

// pausePlacedCaller is one caller a case asks as: the address it asks from, the contract it states
// in pluginContractHeader (0 for none), and whether it is served the keys pauseContract added.
type pausePlacedCaller struct {
	name   string
	remote string
	stated int
	served bool
}

// pausePlacedCallers are the three callers every case asks as. The first two state no contract
// and are placed by their pod alone; the third is in no pod and is placed by its header.
var pausePlacedCallers = []pausePlacedCaller{
	{name: "a pod whose plugin is the contract before pauseContract", remote: statePodAddr + ":41000"},
	{name: "a pod whose plugin is at pauseContract", remote: pauseAtPodAddr + ":41000", served: true},
	{name: "a caller in no pod stating this daemon's contract", remote: "192.0.2.1:41000", stated: DaemonAPIVersion, served: true},
}

// askedAs is a POST of body, as its JSON, to target on handler, asked from caller's address and
// stating caller's contract where it states one.
func askedAs(t *testing.T, handler http.Handler, caller pausePlacedCaller, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the request body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
	request.RemoteAddr = caller.remote
	if caller.stated > 0 {
		request.Header.Set(pluginContractHeader, strconv.Itoa(caller.stated))
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// pauseRefusalServed requires recorder to answer the POST of target with a 409 refusal coded code
// with its sentence (refusalServed), and answers the body's members.
func pauseRefusalServed(t *testing.T, recorder *httptest.ResponseRecorder, target, code string) map[string]json.RawMessage {
	t.Helper()
	// refusalServed decodes the body out of the recorder, so its members are read from a copy.
	encoded := bytes.Clone(recorder.Body.Bytes())
	refusalServed(t, recorder, target, http.StatusConflict, code)
	return keysOf(t, encoded)
}

// A held wake's resume carries `pause`, the key pauseContract added to the release records, which
// the reader of a plugin below that contract does not name. placedByPod places the records'
// caller by its pod, so a pod whose plugin is the contract before is served the resume without
// the key, while a pod at the contract and a caller stating this daemon's contract are served the
// pause that held the wake. Were placedByPod to answer false for the records, the route would
// serve every caller that states no contract this daemon's own body, the key with it.
func TestTheReleaseRecordsPlaceACallerThatStatesNoContractByItsPod(t *testing.T) {
	h := newPauseHarness(t, pauseCapable())
	h.placePodCallers()
	token, bootToken := h.launch("LEGION-208", claim.RoleImplementer)
	registered := h.registered(bootToken, "ses_implementer")
	began := beganPause(t, h.pauseOK(operatorPauseRoute, OperatorPauseRequest{Reason: "a provider outage"}, bearer))

	// A wake the pause held pairs with no release and names the pause (RecordPodWakeTx).
	var resumeID int64
	if err := h.store.Tx(h.ctx, func(tx pgx.Tx) error {
		var (
			taken bool
			err   error
		)
		resumeID, taken, err = store.RecordPodWakeTx(h.ctx, tx, store.PodResume{
			Project: testProject, Issue: "LEGION-209", Role: "tester", Kind: "delivery",
			At: pauseStartsAt.Add(10 * time.Minute), Pause: began.ID,
		})
		if err == nil && !taken {
			t.Error("the held wake was not taken")
		}
		return err
	}); err != nil {
		t.Fatalf("record the held wake: %v", err)
	}
	since := pauseStartsAt.Add(-time.Minute)
	records, err := h.store.PodReleaseRecords(h.ctx, testProject, since)
	if err != nil {
		t.Fatalf("read the release records: %v", err)
	}
	held := false
	for _, resume := range records.Resumes {
		held = held || (resume.ID == resumeID && resume.Pause == began.ID)
	}
	if !held {
		t.Fatalf("the store's records hold no resume %d of pause %d; this test proves nothing: %+v", resumeID, began.ID, records.Resumes)
	}

	for _, caller := range pausePlacedCallers {
		t.Run(caller.name, func(t *testing.T) {
			recorder := askedAs(t, h.handler, caller, releaseRecordsRoute, readRecords(token, registered.Secret, since.Format(time.RFC3339)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("release-records = %d, want 200; body %s", recorder.Code, recorder.Body)
			}
			var body struct {
				Resumes []map[string]json.RawMessage `json:"resumes"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode the records %s: %v", recorder.Body, err)
			}
			var resume map[string]json.RawMessage
			for _, served := range body.Resumes {
				if string(served["id"]) == strconv.FormatInt(resumeID, 10) {
					resume = served
				}
			}
			if resume == nil {
				t.Fatalf("the records serve no resume %d: %s", resumeID, recorder.Body)
			}
			heldBy, carried := resume["pause"]
			if carried != caller.served {
				t.Fatalf("the held resume carries `pause` = %t, want %t: %s", carried, caller.served, recorder.Body)
			}
			if caller.served && string(heldBy) != strconv.FormatInt(began.ID, 10) {
				t.Fatalf("the held resume's pause = %s, want %d", heldBy, began.ID)
			}
		})
	}
}

// A pause refused for the daemon's configuration (here the idle release turned off, on the
// kubernetes runtime whose callers are pods) names the setting in `setting`, a key
// pauseContract added to a refusal. placedByPod places the caller of a PauseFailure by its pod,
// so the controller's plugin in a pod below that contract is served the plain Failure, its `code`
// and `error` alone, while a pod at the contract and a caller stating this daemon's contract are
// served the setting. Were placedByPod to answer false for it, the writer would serve every
// caller that states no contract the setting.
func TestAPauseUnavailableRefusalPlacesACallerThatStatesNoContractByItsPod(t *testing.T) {
	offIdle := pauseCapable()
	offIdle.Runtime.Kubernetes.IdleRelease.IdleMinutes = 0
	h := newPauseHarness(t, offIdle)
	h.placePodCallers()
	grant := h.controllerSession()

	for _, caller := range pausePlacedCallers {
		t.Run(caller.name, func(t *testing.T) {
			recorder := askedAs(t, h.handler, caller, controllerPauseRoute, ProjectPauseRequest{GrantID: grant, Reason: "a provider outage"})
			body := pauseRefusalServed(t, recorder, controllerPauseRoute, pause.CodeUnavailable)
			if !caller.served {
				assertKeys(t, "the refusal served below pauseContract", body, []string{"code", "error"})
				return
			}
			assertKeys(t, "the refusal served at pauseContract", body, []string{"code", "error", "setting"})
			var setting string
			if err := json.Unmarshal(body["setting"], &setting); err != nil || setting != string(pause.SettingIdleReleaseOff) {
				t.Fatalf("the refusal's setting = %s (%v), want %q", body["setting"], err, pause.SettingIdleReleaseOff)
			}
		})
	}
	if n := h.pauseCount(); n != 0 {
		t.Fatalf("the refused pauses recorded %d", n)
	}
}

// While the project is paused, the controller's compaction of a worker's claim is refused
// PROJECT_PAUSED naming the standing pause in `pause`, a key pauseContract added to a refusal
// (refusedWhilePaused). placedByPod places the caller of a PauseFailure by its pod, so the
// controller's plugin in a pod below that contract is served the plain Failure, its `code` and
// `error` alone, while a pod at the contract and a caller stating this daemon's contract are
// served the pause. Were placedByPod to answer false for it, the writer would serve every caller
// that states no contract the pause.
func TestAProjectPausedRefusalPlacesACallerThatStatesNoContractByItsPod(t *testing.T) {
	h := newPauseHarness(t, pauseCapable())
	h.placePodCallers()
	grant := h.controllerSession()
	implementer, _ := h.launch("LEGION-208", claim.RoleImplementer)
	h.ready(implementer, "ses_implementer", 1)
	began := beganPause(t, h.pauseOK(operatorPauseRoute, OperatorPauseRequest{Reason: "a provider outage"}, bearer))

	for _, caller := range pausePlacedCallers {
		t.Run(caller.name, func(t *testing.T) {
			const compactRoute = "/legion/v1/claims/compact"
			recorder := askedAs(t, h.handler, caller, compactRoute,
				ClaimRequest{GrantID: grant, Issue: "LEGION-208", Role: string(claim.RoleImplementer)})
			body := pauseRefusalServed(t, recorder, compactRoute, projectPaused)
			if !caller.served {
				assertKeys(t, "the refusal served below pauseContract", body, []string{"code", "error"})
				return
			}
			assertKeys(t, "the refusal served at pauseContract", body, []string{"code", "error", "pause"})
			var standing PauseView
			if err := json.Unmarshal(body["pause"], &standing); err != nil || standing.ID != began.ID {
				t.Fatalf("the refusal's pause = %s (%v), want pause %d", body["pause"], err, began.ID)
			}
		})
	}
	if _, open := h.svc.Paused(); !open {
		t.Fatal("the pause ended under the refused compactions")
	}
}
