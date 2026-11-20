// packages/envoy/cmd/cipher/grant_test.go
// What one row of `agent-secrets grant list` says about who approved a grant and who decided it,
// and which broker route `agent-secrets grant revoke` takes, run against a stand-in broker.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/helper"
)

// cell is the value of the column named head in a row grantRow built. A name grantColumns does
// not hold is a test bug, not a missing cell.
func cell(t *testing.T, row []string, head string) string {
	t.Helper()
	i := slices.Index(grantColumns, head)
	if i < 0 {
		t.Fatalf("the table has no %s column; its columns are %v", head, grantColumns)
	}
	return row[i]
}

// TestGrantListNamesTheApproverAndTheDecider: the model judge approves a request in the secret's
// owner's stead, and the grant it mints records that owner as its approver — the grant is the one
// the owner would have made. A table that printed the approver alone therefore read as the
// owner's own approval of a request the owner never saw, which is what the broker's decided_by
// is for: the judge's "judge:<model id>", the deciding person's login for a person's approval,
// and nothing at all for a grant the policy gave without asking.
//
// The three rows are the three answers side by side: only the judged one has an approver and a
// decider that differ, and that difference is the reason the two columns are printed together.
func TestGrantListNamesTheApproverAndTheDecider(t *testing.T) {
	const (
		owner = "ada@example.com"
		judge = "judge:claude-opus-4-6"
	)
	owned := func(s string) *string { return &s }
	for _, tc := range []struct {
		name              string
		grant             OperatorGrant
		approver, decider string
	}{
		{
			name:     "the model judge approved it in the owner's stead",
			grant:    OperatorGrant{Granted: "approval", Approver: owned(owner), DecidedBy: owned(judge)},
			approver: owner, decider: judge,
		},
		{
			name:     "the owner approved it herself",
			grant:    OperatorGrant{Granted: "approval", Approver: owned(owner), DecidedBy: owned(owner)},
			approver: owner, decider: owner,
		},
		{
			name:     "the policy gave it without asking",
			grant:    OperatorGrant{Granted: "automatic"},
			approver: "-", decider: "-",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := grantRow(cipheraddr.New("https://broker.example"), tc.grant)
			if len(row) != len(grantColumns) {
				t.Fatalf("the row has %d cells for %d columns: %v under %v", len(row), len(grantColumns), row, grantColumns)
			}
			if got := cell(t, row, "APPROVER"); got != tc.approver {
				t.Errorf("APPROVER = %q, want %q", got, tc.approver)
			}
			if got := cell(t, row, "DECIDED_BY"); got != tc.decider {
				t.Errorf("DECIDED_BY = %q, want %q", got, tc.decider)
			}
		})
	}
}

// grantBroker stands in for the broker `grant revoke` calls. It answers a session's revoke (POST
// /v1/grants/{id}/revoke) with 200, or with 403 NOT_YOURS as the broker answers a grant of
// another enrollment when notYours is set, and the operator's revoke (POST
// /v1/operator/grants/{id}/revoke) with 200. It records every request it is sent and the
// enrollment each session proof names. Any request after a NOT_YOURS answer fails the test: the
// command was refused, and no other route may follow.
type grantBroker struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []string
	signedBy []string
	refused  bool
}

func newGrantBroker(t *testing.T, notYours bool) *grantBroker {
	t.Helper()
	b := &grantBroker{}
	revoked := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"state": "revoked"})
	}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.requests = append(b.requests, r.Method+" "+r.URL.Path)
		b.signedBy = append(b.signedBy, proofEID(r))
		after := b.refused
		b.mu.Unlock()
		if after {
			t.Errorf("%s %s reached the broker after it answered NOT_YOURS: the command tried another route", r.Method, r.URL.Path)
			http.Error(w, "no request may follow NOT_YOURS", http.StatusConflict)
			return
		}
		path, session := strings.CutPrefix(r.URL.Path, "/v1/grants/")
		_, operator := strings.CutPrefix(r.URL.Path, "/v1/operator/grants/")
		switch {
		case r.Method != http.MethodPost || !strings.HasSuffix(path, "/revoke"):
			http.NotFound(w, r)
		case session && notYours:
			b.mu.Lock()
			b.refused = true
			b.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "NOT_YOURS", "error": "this request or grant belongs to another session"})
		case session, operator:
			revoked(w)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(b.server.Close)
	return b
}

func (b *grantBroker) URL() string { return b.server.URL }

// seen is every request the stand-in was sent, as "METHOD PATH", and the enrollment each one's
// session proof named ("" for none).
func (b *grantBroker) seen() (requests, signedBy []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.requests), slices.Clone(b.signedBy)
}

// grantRig is a chainRig, a process's world of its own, told of the stand-in b.
func grantRig(t *testing.T, binary string, b *grantBroker) *chainRig {
	t.Helper()
	r := newChainRig(t, binary, nil)
	r.env["AGENT_SECRETS_URL"] = b.URL()
	return r
}

// launcherProofHelper is a helper that signs the operator's routes for this process
// (sign-launcher), as a real one does for a process outside every session it registered, and
// answers its identity probe (sign) with answer.
func launcherProofHelper(t *testing.T, answer helper.Response) (string, <-chan helper.Request) {
	t.Helper()
	return fakeHelperByOp(t, map[string]helper.Response{
		"sign":          answer,
		"sign-launcher": {OK: true, Proof: "eyJ.launcher.proof", CredentialID: "operator-credential"},
	})
}

// TestGrantRevokeOutsideASessionBesideAnOldMachineSession: a client before #4143 kept the
// machine's own session in the key dir, with the machine-session marker beside it. That is no
// launcher's key, and identity does not read it as a session. So `grant revoke` from the
// operator's own shell beside it takes the operator route, as it did before, and leaves the key
// dir as it found it: moving that session into the machine cache is the use chain's work.
func TestGrantRevokeOutsideASessionBesideAnOldMachineSession(t *testing.T) {
	binary := chainBinary(t)
	broker := newGrantBroker(t, false)
	r := grantRig(t, binary, broker)
	sock, _ := launcherProofHelper(t, helper.Response{Code: helper.CodeNotASession, Error: "pid 5 is not inside a registered machine session"})
	r.env["AGENT_SECRETS_HELPER_SOCK"] = sock
	writeSessionFiles(t, r.keyDir(), "old-machine-enrollment")
	if err := os.WriteFile(filepath.Join(r.keyDir(), "machine-session"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	grantID := uuid.NewString()
	code, stdout, stderr := r.run("grant", "revoke", grantID)
	want := "revoked " + grantID + " at " + brokerSource(cipheraddr.New(broker.URL())).String() + "\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Fatalf("grant revoke beside an old machine session: exit %d, stdout %q, stderr %q; want 0 and %q", code, stdout, stderr, want)
	}
	if requests, _ := broker.seen(); !slices.Equal(requests, []string{"POST /v1/operator/grants/" + grantID + "/revoke"}) {
		t.Fatalf("the broker saw %v; want the operator's revoke alone", requests)
	}
	for _, name := range []string{"key.pem", "enrollment", "machine-session"} {
		if _, err := os.Stat(filepath.Join(r.keyDir(), name)); err != nil {
			t.Fatalf("the key dir lost %s: %v", name, err)
		}
	}
	if _, err := os.Stat(r.machineCache()); !os.IsNotExist(err) {
		t.Fatalf("grant revoke wrote the machine cache (%v); it moves nothing", err)
	}
}

// TestGrantRevokeInsideASessionIsASessionCommand: inside a session, `grant revoke` takes its
// identity as every session command does (sessionContext): it waits while a box's launcher is
// still enrolling the key dir, and a setting it cannot read is a usage error, exit 2, with the
// line `revoke` prints for it.
func TestGrantRevokeInsideASessionIsASessionCommand(t *testing.T) {
	binary := chainBinary(t)

	t.Run("a setting it cannot read", func(t *testing.T) {
		broker := newGrantBroker(t, false)
		r := grantRig(t, binary, broker)
		writeSessionFiles(t, r.keyDir(), "launcher-enrollment")
		r.env["AGENT_SECRETS_ENROLL_WAIT"] = "soon"
		grantID := uuid.NewString()
		revokeCode, _, revokeStderr := r.run("revoke", grantID)
		code, stdout, stderr := r.run("grant", "revoke", grantID)
		want := strings.Replace(revokeStderr, "agent-secrets revoke: ", "agent-secrets grant revoke: ", 1)
		if revokeCode != exitUsageError || !strings.Contains(revokeStderr, "AGENT_SECRETS_ENROLL_WAIT") {
			t.Fatalf("revoke with AGENT_SECRETS_ENROLL_WAIT=soon: exit %d, stderr %q; want %d naming the variable", revokeCode, revokeStderr, exitUsageError)
		}
		if code != exitUsageError || stdout != "" || stderr != want {
			t.Fatalf("grant revoke with AGENT_SECRETS_ENROLL_WAIT=soon: exit %d, stdout %q, stderr %q; want %d and %q", code, stdout, stderr, exitUsageError, want)
		}
		if requests, _ := broker.seen(); len(requests) != 0 {
			t.Fatalf("the broker saw %v; a setting error asks it nothing", requests)
		}
	})

	t.Run("a launcher still enrolling the key dir", func(t *testing.T) {
		broker := newGrantBroker(t, false)
		r := grantRig(t, binary, broker)
		marker := writePendingMarker(t, r.keyDir(), 0)
		fixture := t.TempDir()
		writeSessionFiles(t, fixture, "launcher-enrollment")
		// A day, so that the bound cannot end the wait within the shared test wait; the launcher
		// writes each file once the call says it waits for it (playTheLauncher).
		r.env["AGENT_SECRETS_ENROLL_WAIT"] = "24h"

		grantID := uuid.NewString()
		c := startCall(t, r.command("grant", "revoke", grantID))
		playTheLauncher(t, c, r.keyDir(), fixture, "24h0m0s")
		code := c.end(t)
		want := "revoked " + grantID + " at " + brokerSource(cipheraddr.New(broker.URL())).String() + "\n"
		waited := launcherWaitLine(marker, "24h0m0s") + enrollmentWaitLine
		if stdout, stderr := c.stdout.String(), c.stderr.String(); code != 0 || stdout != want || stderr != waited {
			t.Fatalf("grant revoke while the launcher enrolls: exit %d, stdout %q, stderr %q; want 0, %q once it has, and the wait's two lines %q",
				code, stdout, stderr, want, waited)
		}
		requests, signedBy := broker.seen()
		if !slices.Equal(requests, []string{"POST /v1/grants/" + grantID + "/revoke"}) || !slices.Equal(signedBy, []string{"launcher-enrollment"}) {
			t.Fatalf("the broker saw %v, signed by %v; want the session's revoke, signed by the enrollment the launcher wrote", requests, signedBy)
		}
	})
}

// TestGrantRevokeTriesNoOtherRouteAfterNotYours: once the broker refuses the session's revoke
// NOT_YOURS, `grant revoke` prints that one line, exits 1 and tries no other route. The helper
// here would sign the operator's route, as a real one does for a process outside every session
// it registered, such as one holding a box launcher's key dir on the operator's own machine. So
// a fall-through would reach the stand-in, which fails the test on any request after its
// NOT_YOURS, and would ask the helper for a launcher proof.
func TestGrantRevokeTriesNoOtherRouteAfterNotYours(t *testing.T) {
	binary := chainBinary(t)
	broker := newGrantBroker(t, true)
	r := grantRig(t, binary, broker)
	sock, ops := launcherProofHelper(t, helper.Response{Code: helper.CodeNotASession, Error: "pid 5 is not inside a registered machine session"})
	r.env["AGENT_SECRETS_HELPER_SOCK"] = sock
	writeSessionFiles(t, r.keyDir(), "launcher-enrollment")

	grantID := uuid.NewString()
	code, stdout, stderr := r.run("grant", "revoke", grantID)
	want := notYoursStderr(grantID)
	if code != 1 || stdout != "" || stderr != want {
		t.Fatalf("grant revoke of another enrollment's grant: exit %d, stdout %q, stderr %q; want 1 and exactly %q", code, stdout, stderr, want)
	}
	if requests, _ := broker.seen(); !slices.Equal(requests, []string{"POST /v1/grants/" + grantID + "/revoke"}) {
		t.Fatalf("the broker saw %v; want the session's revoke alone", requests)
	}
	if asked := drainOps(ops); slices.Contains(asked, "sign-launcher") {
		t.Fatalf("the helper was asked %v; after NOT_YOURS no launcher proof may be asked for", asked)
	}
}

// notYoursStderr is the whole of what `grant revoke` prints when the broker refuses it NOT_YOURS:
// the broker ends only a grant of the enrollment that signs, so the line names that and no wider
// "another session".
func notYoursStderr(grantID string) string {
	return "agent-secrets grant revoke: grant " + grantID + " was given to another broker identity (enrollment) than the one this process signs as; " +
		"its holder ends it, or its operator from their own shell (agent-secrets grant revoke " + grantID + "), or its approver on Dispatch's Live grants page\n"
}
