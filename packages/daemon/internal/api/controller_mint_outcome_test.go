package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A controller mint's commit revokes the grants of the generations it replaced. These tests run a
// second server over the same database and the same grants, whose store is reached through a
// proxy that cuts the mint's commit (testdb.CommitFaults): one daemon whose watcher waits between
// its reads of the store while its other connections serve, made deterministic.

// mintRig is the architect harness and a second server over its database and grants, through the
// fault proxy.
type mintRig struct {
	h        *harness
	grants   *credential.Grants
	statuses *statusRecorder
	fault    *testdb.CommitFault
	faulted  http.Handler
	log      *lockedLog
}

func newMintRig(t *testing.T) *mintRig {
	t.Helper()
	grants := credential.New(nil)
	h, _, statuses := newArchitectHarness(t, grants, nil)
	dsn, fault := testdb.CommitFaults(t, h.store.Pool().Config().ConnString())
	faulted, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open the store through the fault proxy: %v", err)
	}
	t.Cleanup(faulted.Close)
	log := &lockedLog{}
	handler := NewServer("127.0.0.1", 8437, Options{
		Supervisor:    h.supervisor,
		BootTokens:    h.tokens,
		Project:       testProject,
		OperatorToken: testOperatorToken,
		Controller:    faulted,
		Grants:        grants,
		Pool:          faulted.Pool(),
		Record:        record.NewStore(),
		Dispatch:      statuses,
		Log:           slog.New(slog.NewTextHandler(log, nil)),
	}).Handler
	return &mintRig{h: h, grants: grants, statuses: statuses, fault: fault, faulted: handler, log: log}
}

// secretVia posts the request mintedSecret posts, through handler.
func secretVia(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(ControllerSecretRequest{PluginContract: DaemonAPIVersion()})
	if err != nil {
		t.Fatalf("encode the request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/legion/v1/controller/secret", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testOperatorToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

// sessionGrant is the session grant of a registered controller.
func (r *mintRig) sessionGrant(t *testing.T, session, secret string) GrantResponse {
	t.Helper()
	recorder := r.h.controllerSessionGrant(session, secret)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the controller session grant of %s = %d; body %s", session, recorder.Code, recorder.Body)
	}
	var grant GrantResponse
	decodeInto(t, recorder, &grant)
	return grant
}

// generation is the controller generation the direct store reads.
func (r *mintRig) generation(t *testing.T) uint64 {
	t.Helper()
	stored, found, err := r.h.store.Controller(context.Background(), testProject)
	if err != nil || !found {
		t.Fatalf("read the controller record = %t, %v", found, err)
	}
	return stored.Generation
}

func TestAControllerMintWhoseOutcomeWasUnknownRevokesTheOldGrantOnceTheStoreAnswers(t *testing.T) {
	rig := newMintRig(t)
	first := rig.h.mintedSecret()
	registration := rig.h.registeredController(first, "ses_first")
	earlier := rig.sessionGrant(t, "ses_first", registration.Secret)

	fired := rig.fault.Cut("insert into controllers", testdb.CutAfterCommitRefusing)
	if recorder := secretVia(t, rig.faulted); recorder.Code != http.StatusInternalServerError {
		t.Fatalf("the mint whose outcome is unknown = %d, want 500; body %s", recorder.Code, recorder.Body)
	}
	testdb.Met(t, fired)
	stored, _, err := rig.h.store.Controller(context.Background(), testProject)
	if err != nil || stored.Generation != 2 || stored.Registered() {
		t.Fatalf("the controller record = %+v, %v; want generation 2 with no registration: the mint committed", stored, err)
	}
	if _, err := rig.grants.Redeem(earlier.GrantID); err != nil {
		t.Fatalf("the earlier grant before the store answers = %v, want it redeemed: the hook waits", err)
	}

	rig.fault.Refuse(false)
	testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
		"the earlier grant to be revoked once the store says the mint committed", func() bool {
			_, err := rig.grants.Redeem(earlier.GrantID)
			return errors.Is(err, credential.ErrUnavailable)
		})
	status := rig.h.request(http.MethodPost, "/legion/v1/issues/status",
		IssueStatusRequest{GrantID: earlier.GrantID, Issue: "LEGION-208", Status: "backlog"}, nil)
	refused(t, status, http.StatusForbidden, "GRANT_UNAVAILABLE")
	if len(rig.statuses.writes) != 0 {
		t.Fatalf("Dispatch writes = %+v, want none from a revoked controller grant", rig.statuses.writes)
	}
	if got := rig.generation(t); got != 2 {
		t.Fatalf("the controller generation = %d, want 2: no second mint ran", got)
	}
}

func TestADelayedOlderMintConfirmationLeavesANewerMintsGrantsStanding(t *testing.T) {
	rig := newMintRig(t)
	first := rig.h.mintedSecret()
	registration := rig.h.registeredController(first, "ses_first")
	rig.sessionGrant(t, "ses_first", registration.Secret)

	fired := rig.fault.Cut("insert into controllers", testdb.CutAfterCommitRefusing)
	if recorder := secretVia(t, rig.faulted); recorder.Code != http.StatusInternalServerError {
		t.Fatalf("mint A, whose outcome is unknown = %d, want 500; body %s", recorder.Code, recorder.Body)
	}
	testdb.Met(t, fired)
	second := rig.h.mintedSecret()
	if got := rig.generation(t); got != 3 {
		t.Fatalf("the generation after mint B = %d, want 3", got)
	}
	replacement := rig.h.registeredController(second, "ses_second")
	current := rig.sessionGrant(t, "ses_second", replacement.Secret)
	operator := controllerGrant(t, rig.h)

	rig.fault.Refuse(false)
	testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
		"mint A's hook once the store says it committed", func() bool {
			return len(rig.log.lines("api: minted a controller capability; the previous controller's registration and grants are revoked", "generation=2")) == 1
		})
	for _, held := range []struct {
		name string
		id   string
	}{{"the newer session's grant", current.GrantID}, {"the operator's grant", operator.GrantID}} {
		if _, err := rig.grants.Redeem(held.id); err != nil {
			t.Errorf("%s after mint A's late hook = %v, want it to stand", held.name, err)
		}
	}
	status := rig.h.request(http.MethodPost, "/legion/v1/issues/status",
		IssueStatusRequest{GrantID: current.GrantID, Issue: "LEGION-208", Status: "backlog"}, nil)
	if status.Code != http.StatusOK {
		t.Fatalf("issue status with the newer session's grant = %d; body %s", status.Code, status.Body)
	}
}
