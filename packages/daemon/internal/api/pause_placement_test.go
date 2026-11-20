package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The release records and every PauseFailure gained keys at pauseContract and answer from no
// snapshot of their own, so their writers read the project's claims for them (placingClaims) and
// hand those in, and a caller that states no contract is placed by its pod there as on the state.
// These tests drive each writer through the server's handler as three callers: a pod whose plugin
// is the contract before pauseContract, a pod at it, and a caller in no pod that states this
// daemon's contract.

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
// daemon over the harness's pause service, which keeps the release records too
// (Options.ReleaseRecords). The grants are this server's own, so a test mints its grants after it.
func (h *pauseHarness) placePodCallers() {
	h.t.Helper()
	pods := map[netip.Addr]string{
		netip.MustParseAddr(statePodAddr):   statePodUID,
		netip.MustParseAddr(pauseAtPodAddr): pauseAtPodUID,
	}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: podSupervisor{testSupervisor: h.supervisor, claims: pausePodClaims()}, BootTokens: h.tokens,
		Project: testProject, DispatchProject: testDispatchProject, OperatorToken: testOperatorToken, Controller: h.store,
		Grants: credential.New(nil), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store, ReleaseRecords: h.store,
		Handlers: []intake.Handler{&factRecorder{}}, Dispatch: h.statuses, Pause: h.svc,
		CallerPod: func(addr netip.Addr) (string, bool) {
			pod, inPod := pods[addr]
			return pod, inPod
		},
	}).Handler
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

// askedAs is a POST of body, as its JSON, to target on handler, asked from caller's address,
// stating caller's contract where it states one, and carrying header's values beside it.
func askedAs(t *testing.T, handler http.Handler, caller pausePlacedCaller, target string, body any, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the request body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
	request.RemoteAddr = caller.remote
	for name, values := range header {
		request.Header[name] = values
	}
	if caller.stated > 0 {
		request.Header.Set(pluginContractHeader, strconv.Itoa(caller.stated))
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// pauseRefusalServed requires recorder to be a 409 refusal coded code with its sentence, and
// answers the body's members.
func pauseRefusalServed(t *testing.T, recorder *httptest.ResponseRecorder, code string) map[string]json.RawMessage {
	t.Helper()
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, http.StatusConflict, recorder.Body)
	}
	body := keysOf(t, recorder.Body.Bytes())
	var refused Failure
	if err := json.Unmarshal(recorder.Body.Bytes(), &refused); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body, err)
	}
	if refused.Code != code || refused.Error == "" {
		t.Fatalf("the refusal %s, want code %s and a sentence", recorder.Body, code)
	}
	return body
}

// A held wake's resume carries `pause`, the key pauseContract added to the release records, which
// the reader of a plugin below that contract does not name. The route hands in the claims that
// place its caller (releaseRecords, placingClaims), so a pod whose plugin is the contract before
// is served the resume without the key, while a pod at the contract and a caller stating this
// daemon's contract are served the pause that held the wake. Handed no claims, the route would
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
			recorder := askedAs(t, h.handler, caller, releaseRecordsRoute, readRecords(token, registered.Secret, since.Format(time.RFC3339)), nil)
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
// pauseContract added to a refusal. writePauseFailure hands in the claims that place its caller
// (placingClaims), so the controller's plugin in a pod below that contract is served the plain
// Failure, its `code` and `error` alone, while a pod at the contract and a caller stating this
// daemon's contract are served the setting. Handed no claims, the writer would serve every
// caller that states no contract the setting.
func TestAPauseUnavailableRefusalPlacesACallerThatStatesNoContractByItsPod(t *testing.T) {
	offIdle := pauseCapable()
	offIdle.Runtime.Kubernetes.IdleRelease.IdleMinutes = 0
	h := newPauseHarness(t, offIdle)
	h.placePodCallers()
	grant := h.controllerSession()

	for _, caller := range pausePlacedCallers {
		t.Run(caller.name, func(t *testing.T) {
			recorder := askedAs(t, h.handler, caller, controllerPauseRoute, ProjectPauseRequest{GrantID: grant, Reason: "a provider outage"}, nil)
			body := pauseRefusalServed(t, recorder, pause.CodeUnavailable)
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
// (refusedWhilePaused). writePauseFailure hands in the claims that place its caller
// (placingClaims), so the controller's plugin in a pod below that contract is served the plain
// Failure, its `code` and `error` alone, while a pod at the contract and a caller stating this
// daemon's contract are served the pause. Handed no claims, the writer would serve every caller
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
			recorder := askedAs(t, h.handler, caller, "/legion/v1/claims/compact",
				ClaimRequest{GrantID: grant, Issue: "LEGION-208", Role: string(claim.RoleImplementer)}, nil)
			body := pauseRefusalServed(t, recorder, projectPaused)
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
