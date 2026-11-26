// packages/envoy/cmd/cipher/reason_projects_linux_test.go
//go:build linux

// Round 4's B9 on cybermonklabs/legion#4135 through the CLI: the real agent-secrets, a real helper
// and a real broker (ciphertest.NewRig), as machine_linux_test.go drives them. Linux-only for the
// real helper's pidfd pinning.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/helper"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestARequestWhoseReasonNamesMoreThanSixteenProjectsIsRefusedAtTheCLI: on a broker that holds a
// project-access client, `agent-secrets request` with a reason naming the issue keys of 17
// projects exits 1 with the broker's refusal, which names the bound, and writes nothing; the same
// request naming 16 is granted at once. A broker with no such client takes either
// (requests.TestABrokerWithNoProjectAccessTakesAReasonNamingAnyNumberOfProjects).
func TestARequestWhoseReasonNamesMoreThanSixteenProjectsIsRefusedAtTheCLI(t *testing.T) {
	// Not parallel: it signals the test process or changes its signal handling (main.serveRealHelper, main.serveRealHelperWith, (*Server).Serve, (*Server).handle, (*Server).unenrollSessionKey, (*Broker).Revoke, (*Broker).signingAuthority, (*SignInAuthority).Current, (*Client).Token, (*Client).tradeHeld, (*Client).trade, (*ExternalAccountIdentity).Token, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.serveRealHelper, main.serveRealHelperWith, (*Server).Serve, (*Server).handle, (*Server).unenrollSessionKey, (*Broker).Revoke, (*Broker).signingAuthority, (*SignInAuthority).Current, (*Client).Token, (*Client).tradeHeld, (*Client).trade, (*ExternalAccountIdentity).Token, (gcloudToken).Token, main.runGcloud, main.runRecorded, runningGcloud set at secret_gcp.go:516).
	dispatch := projectaccesstest.NewServer(t, nil, nil)
	rig := ciphertest.NewRig(t, ciphertest.WithProjectAccess(projectaccess.New(dispatch.URL, dispatch.Account, nil)))
	binary := buildAgentSecrets(t)
	_, sock := serveRealHelper(t, rig.URL, rig.Operator)
	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("machine login (code %s): exit %d, stderr %q", code, exit, stderr)
	}
	boxKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&boxKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := helper.Call(sock, helper.Request{Op: "enroll-session-key", RuntimeID: "reason-box-" + t.Name(), Thumbprint: thumbprint}, untilGiveUp(t))
	if err != nil || !enrolled.OK {
		t.Fatalf("enroll-session-key: %+v %v", enrolled, err)
	}
	keyDir := writeKeyDir(t, boxKey, enrolled.EnrollmentID)
	reason := func(projects int) string {
		keys := make([]string, projects)
		for i := range keys {
			keys[i] = fmt.Sprintf("P%02d-1", i)
		}
		return strings.Join(keys, " ")
	}

	stdout, stderr, exit := runAgentSecrets(t, binary, rig.URL, keyDir, nil, "request", "TEST_AUTO_SECRET", "--json", "--reason", reason(record.MaxReasonProjects+1))
	if want := fmt.Sprintf("a request may name at most %d (REQUEST_INVALID)", record.MaxReasonProjects); exit != 1 || !strings.Contains(stderr, want) {
		t.Fatalf("request with a reason naming %d projects: exit %d, stdout %q, stderr %q; want exit 1 and %q", record.MaxReasonProjects+1, exit, stdout, stderr, want)
	}
	t.Logf("the refusal: %s", strings.TrimSpace(stderr))

	stdout, stderr, exit = runAgentSecrets(t, binary, rig.URL, keyDir, nil, "request", "TEST_AUTO_SECRET", "--json", "--reason", reason(record.MaxReasonProjects))
	var granted RequestResult
	if err := json.Unmarshal([]byte(stdout), &granted); exit != 0 || err != nil || granted.State != "granted" {
		t.Fatalf("request with a reason naming %d projects: exit %d, stdout %q, stderr %q; want it granted at once", record.MaxReasonProjects, exit, stdout, stderr)
	}
}
