package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// podUpdateAnswer is the round the stand-in daemon of these tests answers a request it takes.
var podUpdateAnswer = PodUpdateResponse{
	Round:   "pod:9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22",
	Reasons: "the project's controller asked for the replacement of this pod",
	Opened:  true,
}

// podUpdateHarness is newHarness's daemon serving the controller's pod update with the daemon's
// own opening of the round (the daemon's requestPodUpdate, which this package has none of) stood
// in for by open, and the deployment's Dispatch project set to LEGION, so an issue of another
// project is one the route can tell by its key. asked is each issue the stand-in was asked for,
// in order, and logs is what the server logged.
func podUpdateHarness(t *testing.T, open func(issue string) (PodUpdateResponse, error)) (*harness, *[]string, *bytes.Buffer) {
	t.Helper()
	h := newHarness(t)
	asked, logs := &[]string{}, &bytes.Buffer{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		OperatorToken: testOperatorToken, Controller: h.store, Pool: h.store.Pool(), Record: record.NewStore(),
		Releaser: h.runtime, Trees: h.store, Log: slog.New(slog.NewJSONHandler(logs, nil)),
		RequestPodUpdate: func(_ context.Context, issue string) (PodUpdateResponse, error) {
			*asked = append(*asked, issue)
			return open(issue)
		},
	}).Handler
	return h, asked, logs
}

// askPodUpdate posts the controller's pod update of issue under grant.
func askPodUpdate(h *harness, grant, issue string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/issues/pod-update", PodUpdateRequest{GrantID: grant, Issue: issue}, nil)
}

// The route takes the controller's grant alone, a Dispatch issue key, and an issue of the
// daemon's own project, and each refusal is made before the daemon is asked anything: a phase
// worker's grant is 403 CONTROLLER_REQUIRED, a key that is no issue key 400 INVALID_ISSUE, and an
// issue of another project 404 NO_CLAIM, the code the claim operations give the one form another
// project's claim takes there, a claim this daemon does not supervise.
//
// The negative control is any of the three checks undone: the stand-in daemon is then asked, and
// answers the round with 200.
func TestThePodUpdateNeedsAControllerGrantAnIssueKeyAndAnIssueOfTheDaemonsProject(t *testing.T) {
	h, asked, _ := podUpdateHarness(t, func(string) (PodUpdateResponse, error) { return podUpdateAnswer, nil })
	token, _ := h.launchIn("LEGION-208", "LEGION-209", claim.RoleImplementer)
	registered := h.registered(h.bootToken(token), "ses_implementer")
	var worker GrantResponse
	minted := h.request(http.MethodPost, "/legion/v1/grants", GrantRequest{
		SessionID: "ses_implementer", Secret: registered.Secret, Tree: "LEGION-208", Issue: "LEGION-209",
	}, nil)
	if minted.Code != http.StatusOK {
		t.Fatalf("mint the worker's grant = %d; body %s", minted.Code, minted.Body)
	}
	decodeInto(t, minted, &worker)

	refused(t, askPodUpdate(h, worker.GrantID, "LEGION-209"), http.StatusForbidden, "CONTROLLER_REQUIRED")
	controller := controllerGrant(t, h).GrantID
	refused(t, askPodUpdate(h, controller, "legion-209"), http.StatusBadRequest, "INVALID_ISSUE")
	refused(t, askPodUpdate(h, controller, "OTHER-209"), http.StatusNotFound, "NO_CLAIM")
	if len(*asked) != 0 {
		t.Fatalf("the daemon was asked to open a round over the pods of %v, want no request of the three to reach it", *asked)
	}

	// The same controller grant over an issue of the daemon's own project is the daemon's to answer.
	if recorder := askPodUpdate(h, controller, "LEGION-209"); recorder.Code != http.StatusOK {
		t.Fatalf("the controller's pod update of LEGION-209 = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	if !slices.Equal(*asked, []string{"LEGION-209"}) {
		t.Fatalf("the daemon was asked for %v, want LEGION-209 alone", *asked)
	}
}

// An architect's grant is refused 403 CONTROLLER_REQUIRED, as a phase worker's is, though it opens
// the claim door's six operations for the claims of the issues it owns: the pod update is the
// controller's alone, and the daemon is asked nothing. The architect is LEGION-208's root
// architect, built as the claim door's tests build it (treeArchitect), asking for the pod of its
// own issue.
//
// The negative control is the route taking any grant (no grant.Controller check): the daemon is
// asked for the pod of LEGION-208, and the route answers the round with 200.
func TestThePodUpdateRefusesAnArchitectsGrant(t *testing.T) {
	h, asked, _ := podUpdateHarness(t, func(string) (PodUpdateResponse, error) { return podUpdateAnswer, nil })
	architect := treeArchitect(t, h)

	refused(t, askPodUpdate(h, architect.grant(t), "LEGION-208"), http.StatusForbidden, "CONTROLLER_REQUIRED")
	if len(*asked) != 0 {
		t.Fatalf("the daemon was asked to open a round over the pods of %v, want the architect's request to reach it not at all", *asked)
	}
}

// What the daemon answers is the route's answer: the round with 200, and one log line naming the
// caller, the issue and the round; each refusal with its own status and code and the daemon's
// sentence unchanged, since that sentence names the issue, the pod or the limit, and a Retry-After
// in whole seconds where the refusal names a wait and none where it names none; and a failure of
// the daemon's own read with 500 POD_UPDATE_FAILED.
//
// The negative control is the refusals answered as one failure (every one 500): each row of the
// table below names the code it reads.
func TestThePodUpdateAnswersTheDaemonsRoundOrItsRefusal(t *testing.T) {
	refusals := map[string]struct {
		refused    *PodUpdateRefused
		status     int
		code       string
		retryAfter string
	}{
		"LEGION-210": {&PodUpdateRefused{Refusal: PodUpdateNoClaim, Message: "this daemon supervises no claim of LEGION-210"}, http.StatusNotFound, "NO_CLAIM", ""},
		"LEGION-211": {&PodUpdateRefused{Refusal: PodUpdateNoPod, Message: "no claim of LEGION-211 records a process in a pod"}, http.StatusNotFound, "NO_POD", ""},
		"LEGION-212": {&PodUpdateRefused{Refusal: PodUpdatePodChanging, Message: "the claims of LEGION-212 record two pods"}, http.StatusConflict, "POD_CHANGING", ""},
		"LEGION-213": {&PodUpdateRefused{Refusal: PodUpdateLimited, Message: "the project's pod_update_max_open_rounds (1) holds back the update round of LEGION-213"}, http.StatusConflict, "POD_UPDATE_LIMITED", ""},
		"LEGION-215": {&PodUpdateRefused{Refusal: PodUpdateNotRecorded, Message: "no claim of LEGION-215 recorded the update round within 30s"}, http.StatusServiceUnavailable, "POD_UPDATE_NOT_RECORDED", ""},
		"LEGION-216": {&PodUpdateRefused{Refusal: PodUpdateBusy, Message: "a sweep of this daemon's update rounds held its walk over the project's issues until the request for LEGION-216 ended", RetryAfter: time.Minute},
			http.StatusServiceUnavailable, "POD_UPDATE_BUSY", "60"},
	}
	h, _, logs := podUpdateHarness(t, func(issue string) (PodUpdateResponse, error) {
		if row, refused := refusals[issue]; refused {
			return PodUpdateResponse{}, row.refused
		}
		if issue == "LEGION-214" {
			return PodUpdateResponse{}, errors.New("read the claims of LEGION-214: the store did not answer")
		}
		return podUpdateAnswer, nil
	})
	controller := controllerGrant(t, h).GrantID

	recorder := askPodUpdate(h, controller, "LEGION-209")
	if recorder.Code != http.StatusOK {
		t.Fatalf("the pod update of LEGION-209 = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var answer PodUpdateResponse
	decodeInto(t, recorder, &answer)
	if answer != podUpdateAnswer {
		t.Fatalf("the answer = %+v, want the daemon's %+v", answer, podUpdateAnswer)
	}
	var lines []map[string]any
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if !strings.Contains(line, `"msg":"api: the controller asked for the replacement of an issue's pod"`) {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("decode the log line %s: %v", line, err)
		}
		lines = append(lines, fields)
	}
	if len(lines) != 1 || lines[0]["issue"] != "LEGION-209" || lines[0]["round"] != podUpdateAnswer.Round || lines[0]["opened"] != true {
		t.Fatalf("the route logged %v in\n%s\nwant one line naming the controller, LEGION-209 and round %s opened", lines, logs, podUpdateAnswer.Round)
	}

	for issue, row := range refusals {
		refused := askPodUpdate(h, controller, issue)
		var failure Failure
		decodeInto(t, refused, &failure)
		if refused.Code != row.status || failure.Code != row.code || failure.Error != row.refused.Message {
			t.Errorf("the refusal of %s = %d %s %q, want %d %s with the daemon's own %q",
				issue, refused.Code, failure.Code, failure.Error, row.status, row.code, row.refused.Message)
		}
		if got := refused.Header().Get("Retry-After"); got != row.retryAfter {
			t.Errorf("the refusal of %s carries Retry-After %q, want %q", issue, got, row.retryAfter)
		}
	}
	refused(t, askPodUpdate(h, controller, "LEGION-214"), http.StatusInternalServerError, "POD_UPDATE_FAILED")
}
