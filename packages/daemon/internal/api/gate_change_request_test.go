package api

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// changeRequestRoot is the root every case registers, built at run time from the harness's
// project and a number.
var changeRequestRoot = fmt.Sprintf("%s-%d", "LEGION", 208)

// The two documents of the root: its spec, the issue's primary document, and its Verification
// document.
const (
	changeRequestSpec         = "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093"
	changeRequestVerification = "4a6b8c0d-2e4f-4a6b-8c0d-2e4f6a8b0c1d"
)

// specGated is the deployment that gates the spec step alone, `gates.design: root-issues`.
var specGated = []record.DesignStep{record.DesignStepSpec}

// register posts one registration of document at version for step ("" for none) on h, and
// answers the response.
func registerChangeRequestGate(t *testing.T, h *harness, architect liveClaim, document string, version int, step record.DesignStep) int {
	t.Helper()
	body := map[string]any{"grantId": architect.grant(t), "issue": changeRequestRoot, "artifactId": document, "version": version}
	if step != "" {
		body["step"] = step
	}
	return h.request(http.MethodPost, "/legion/v1/gates/register", body, nil).Code
}

// A registration of the spec step of a deployment that gates it reads the issue's change request
// at the version the gate is registered at, the newer of the caller's and Dispatch's (R23), and
// carries what it read, so the workflow fixes the gate's rule from it: whether the repository is a
// spec source and whether that version proposes nothing.
func TestARegistrationOfTheSpecStepReadsTheChangeRequestAtTheRegisteredVersion(t *testing.T) {
	h, facts, statuses := newArchitectHarnessGating(t, specGated, nil, nil)
	statuses.documents = map[string]string{changeRequestSpec: changeRequestRoot}
	statuses.approvals = map[string]dispatch.Approval{changeRequestSpec: {State: "none", LatestVersion: 5}}
	statuses.changeRequests = map[string]dispatch.ChangeRequest{changeRequestRoot: {SpecSource: true}}
	seedTree(t, h, changeRequestRoot)
	architect := newLiveClaim(t, h, changeRequestRoot, claim.RoleArchitect)
	if code := registerChangeRequestGate(t, h, architect, changeRequestSpec, 4, ""); code != http.StatusOK {
		t.Fatalf("register the spec = %d, want 200", code)
	}
	if want := []string{changeRequestRoot + "@5"}; !reflect.DeepEqual(statuses.changeRequestReads, want) {
		t.Fatalf("change request reads = %q, want %q: the registered version is Dispatch's 5, above the caller's 4", statuses.changeRequestReads, want)
	}
	want := []intake.Fact{intake.GateRegistered{Issue: changeRequestRoot, Step: record.DesignStepSpec, ArtifactID: changeRequestSpec, Version: 4,
		DispatchVersion: 5, Since: record.EventMark{Seq: 7},
		ChangeRequest: &intake.ChangeRequestRead{Version: 5, SpecSource: true, ProposesNothing: true}}}
	if got := facts.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("gate facts = %#v, want %#v", got, want)
	}
}

// A Dispatch older than the route answers its router's 404, and the registration carries no
// change request, so the gate keeps today's rule; a version that proposes something is carried
// as one.
func TestARegistrationAgainstAnOlderDispatchCarriesNoChangeRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers map[string]dispatch.ChangeRequest
		want    *intake.ChangeRequestRead
	}{
		{name: "an older Dispatch", answers: nil, want: nil},
		{name: "a version that proposes a statement", answers: map[string]dispatch.ChangeRequest{changeRequestRoot: {SpecSource: true, Proposes: true}},
			want: &intake.ChangeRequestRead{Version: 3, SpecSource: true}},
		{name: "a repository that is no spec source", answers: map[string]dispatch.ChangeRequest{changeRequestRoot: {}},
			want: &intake.ChangeRequestRead{Version: 3, ProposesNothing: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts, statuses := newArchitectHarnessGating(t, specGated, nil, nil)
			statuses.documents = map[string]string{changeRequestSpec: changeRequestRoot}
			statuses.approvals = map[string]dispatch.Approval{changeRequestSpec: {State: "none", LatestVersion: 3}}
			statuses.changeRequests = tc.answers
			seedTree(t, h, changeRequestRoot)
			architect := newLiveClaim(t, h, changeRequestRoot, claim.RoleArchitect)
			if code := registerChangeRequestGate(t, h, architect, changeRequestSpec, 3, ""); code != http.StatusOK {
				t.Fatalf("register the spec = %d, want 200", code)
			}
			got := facts.recorded()
			if len(got) != 1 {
				t.Fatalf("gate facts = %#v, want one", got)
			}
			if carried := got[0].(intake.GateRegistered).ChangeRequest; !reflect.DeepEqual(carried, tc.want) {
				t.Fatalf("the registration carries %+v, want %+v", carried, tc.want)
			}
		})
	}
}

// A read Dispatch fails is no answer: the registration records nothing, so the architect's
// register_gate retries, as a failed read of the approval does. A read Dispatch refuses for good,
// such as a version whose text does not parse, is refused by name with what Dispatch said.
func TestARegistrationWhoseChangeRequestReadFailsRecordsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"Dispatch's trouble", &dispatch.Error{Status: http.StatusBadGateway, Message: "bad gateway"}, http.StatusBadGateway, "DISPATCH_FAILED"},
		{"a version Dispatch cannot read", &dispatch.Error{Status: http.StatusConflict, Code: "CHANGE_REQUEST_UNREADABLE", Message: "the text does not parse"},
			http.StatusConflict, "CHANGE_REQUEST_UNREADABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts, statuses := newArchitectHarnessGating(t, specGated, nil, nil)
			statuses.documents = map[string]string{changeRequestSpec: changeRequestRoot}
			statuses.approvals = map[string]dispatch.Approval{changeRequestSpec: {State: "none", LatestVersion: 3}}
			statuses.changeRequestErr = tc.err
			seedTree(t, h, changeRequestRoot)
			architect := newLiveClaim(t, h, changeRequestRoot, claim.RoleArchitect)
			refused(t, h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
				"grantId": architect.grant(t), "issue": changeRequestRoot, "artifactId": changeRequestSpec, "version": 3,
			}, nil), tc.status, tc.code)
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("gate facts = %#v, want none for a registration whose read failed", got)
			}
		})
	}
}

// Only the spec step of a deployment that gates it holds a change request: a registration of the
// criteria step, and one under a deployment that gates no step, read none.
func TestOnlyTheGatedSpecStepReadsTheChangeRequest(t *testing.T) {
	t.Run("the criteria step", func(t *testing.T) {
		h, facts, statuses := newArchitectHarnessGating(t, []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec}, nil, nil)
		statuses.documents = map[string]string{changeRequestVerification: changeRequestRoot}
		statuses.approvals = map[string]dispatch.Approval{changeRequestVerification: {State: "none", LatestVersion: 2, Role: dispatch.ArtifactRoleVerification}}
		statuses.changeRequests = map[string]dispatch.ChangeRequest{changeRequestRoot: {SpecSource: true}}
		seedTree(t, h, changeRequestRoot)
		architect := newLiveClaim(t, h, changeRequestRoot, claim.RoleArchitect)
		if code := registerChangeRequestGate(t, h, architect, changeRequestVerification, 2, record.DesignStepCriteria); code != http.StatusOK {
			t.Fatalf("register the criteria = %d, want 200", code)
		}
		if len(statuses.changeRequestReads) != 0 || len(facts.recorded()) != 1 || facts.recorded()[0].(intake.GateRegistered).ChangeRequest != nil {
			t.Fatalf("the criteria step read %q and carried %#v, want no read", statuses.changeRequestReads, facts.recorded())
		}
	})
	t.Run("a deployment that gates no step", func(t *testing.T) {
		h, facts, statuses := newArchitectHarness(t, nil, nil)
		statuses.documents = map[string]string{changeRequestSpec: changeRequestRoot}
		statuses.approvals = map[string]dispatch.Approval{changeRequestSpec: {State: "none", LatestVersion: 2}}
		statuses.changeRequests = map[string]dispatch.ChangeRequest{changeRequestRoot: {SpecSource: true}}
		seedTree(t, h, changeRequestRoot)
		architect := newLiveClaim(t, h, changeRequestRoot, claim.RoleArchitect)
		if code := registerChangeRequestGate(t, h, architect, changeRequestSpec, 2, ""); code != http.StatusOK {
			t.Fatalf("register the spec = %d, want 200", code)
		}
		if len(statuses.changeRequestReads) != 0 || len(facts.recorded()) != 1 || facts.recorded()[0].(intake.GateRegistered).ChangeRequest != nil {
			t.Fatalf("a deployment gating no step read %q and carried %#v, want no read", statuses.changeRequestReads, facts.recorded())
		}
	})
}
