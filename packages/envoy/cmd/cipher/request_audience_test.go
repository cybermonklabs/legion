//go:build unix

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestARequestObjectNamesTheBrokersURL: a request object a session's key signs for the broker at
// AGENT_SECRETS_URL (fileSigner.SignRequestObject, through signRequestFor) names proof.Audience of
// the address as its audience - the scheme, host, port and path, with none of the query, user
// information and fragment the address carries - so the broker, which verifies it for its own
// public URL exactly (record.VerifyRequestObject), takes it (query, user-information, fragment),
// where it named the address's whole text and was refused. Control: an address with none of the
// three is the audience as written (plain).
func TestARequestObjectNamesTheBrokersURL(t *testing.T) {
	const public = "https://broker.example:8443/broker"
	for _, c := range []struct{ name, address string }{
		{"query", public + "?sig=k3y-7Qz4k"},
		{"user-information", "https://" + "ada:pass-7Qz4k@broker.example:8443/broker"},
		{"fragment", public + "#pane"},
		{"plain", public},
	} {
		t.Run(c.name, func(t *testing.T) {
			key, err := proof.NewKey()
			if err != nil {
				t.Fatal(err)
			}
			signed, err := (&fileSigner{key: key, enrollmentID: "enr-1"}).SignRequestObject(cipheraddr.New(c.address), []string{"SOME_SECRET"}, "need it", nil, nil)
			if err != nil {
				t.Fatalf("SignRequestObject: %v", err)
			}
			if _, err := record.VerifyRequestObject(signed, public, time.Minute, time.Now()); err != nil {
				t.Errorf("the request object signed at an address %q = %v; want it verified for %q", c.address, err, public)
			}
		})
	}
}

// TestAPodsLoginRequestNamesTheBrokersURL: `enroll --pod` (enrolPod) signs its token login's
// request object for proof.Audience of AGENT_SECRETS_URL, so at an address with user information
// and a query the broker, which verifies the object for its own public URL exactly, takes it and
// the pod enrols (query), where the object named the address's whole text. Control: at the
// broker's own URL the pod enrols as before (plain). The stand-in (servePodBroker) records the
// token login's request object, whatever path it reached.
func TestAPodsLoginRequestNamesTheBrokersURL(t *testing.T) {
	for _, c := range []struct{ name, userInformation, query string }{
		{"query", "ada:pass-7Qz4k@", "?sig=k3y-7Qz4k"},
		{"plain", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			broker, calls := servePodBroker(t, "enrollment-of-the-pod")
			address := "http://" + c.userInformation + strings.TrimPrefix(broker.URL, "http://") + c.query
			t.Setenv("AGENT_SECRETS_URL", address)
			t.Setenv("AGENT_SECRETS_KEY_DIR", t.TempDir())
			var stdout, stderr bytes.Buffer
			code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))}, &stdout, &stderr)
			got := calls()
			if len(got) == 0 {
				t.Fatalf("the pod sent the stand-in nothing: exit %d, stderr %q", code, stderr.String())
			}
			if _, err := record.VerifyRequestObject(got[0].body["request"], broker.URL, time.Minute, time.Now()); err != nil {
				t.Errorf("the pod's login request object at the %s address = %v; want it verified for %q", c.name, err, broker.URL)
			}
			if code != 0 {
				t.Errorf("enroll --pod at the %s address: exit %d, stderr %q; want 0", c.name, code, stderr.String())
			}
		})
	}
}
