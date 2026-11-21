//go:build multiprocess

// The machine sign-in check on both pods of a machine: a caller presenting an access token the
// secrets broker issued for this machine's own Google identity (brokerauth.go, over
// internal/ciphertoken) is judged by the active pod and by the standby alike, since every pod
// serves /v1 and runs the same credential chain (apiAuth). The broker is the stub every other
// row of this credential uses (internal/ciphertoken/ciphertokentest), and the caller's proofs
// come from the transport a real client signs with.

package main

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
)

// mpSignedCall sends one POST to the pod presenting token under the DPoP scheme, with a proof
// this caller signs fresh for it, as a client holding a broker token does. The proof names
// listenerBrokerOrigin, which both pods are configured with (brokerProcessEnv).
func mpSignedCall(t *testing.T, p *mpPod, caller *ciphertokentest.Caller, token, path, body string) mpAnswer {
	t.Helper()
	signed := caller.Signed(t, http.MethodPost, listenerBrokerOrigin+path, token)
	start := time.Now()
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(p.port)+path, strings.NewReader(body))
	if err != nil {
		return mpAnswer{err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", signed.Header.Get("Authorization"))
	request.Header.Set("DPoP", signed.Header.Get("DPoP"))
	response, err := mpHTTP.Do(request)
	if err != nil {
		return mpAnswer{err: err, took: time.Since(start)}
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(response.Body)
	return mpAnswer{status: response.StatusCode, header: response.Header, body: string(data), err: readErr, took: time.Since(start)}
}

// A machine signs in with its own identity and is answered by whichever pod of its machine takes
// the call. The active pod and the standby both take the broker's word for the caller: a token
// naming that machine's session is served on the route it acts as that session on, and a token
// the broker does not know is refused 401 by both, so a standby neither admits what the active
// pod refuses nor refuses what it admits. A SIGKILL of the active pod changes none of it: the
// standby serves /v1 before it holds the machine's delivery lease, so the signed-in caller is
// answered within 10 s of the kill, long before the lease's own take-over bound.
func TestTwoPodsAMachineSignInIsAnsweredByTheActivePodAndTheStandby(t *testing.T) {
	uri := mpNATS(t)
	binary := cmdtest.Build(t, "envoy-listener")
	broker := ciphertokentest.NewStubBroker(t)
	caller := ciphertokentest.NewCaller(t)
	const (
		machineSession = "ses_mp_machine"
		signedIn       = "access-token-of-this-machines-identity"
		notSignedIn    = "access-token-the-broker-never-issued"
		route          = "/v1/interests/subscribe"
	)
	// What the broker answers for the machine's own token: a live session naming the fleet's
	// service and no person, which is a machine nobody has signed in on.
	broker.Says(signedIn, serviceAnswer(machineSession, "example-fleet", caller.JKT()))
	body := mpOnConsumer(t, `{"session_id":"`+machineSession+`","topics":[]}`)

	a, b := mpStartPair(t, binary, uri, brokerProcessEnv(t, broker)...)

	for _, pod := range []*mpPod{a, b} {
		answer := mpSignedCall(t, pod, caller, signedIn, route, body)
		if answer.err != nil || answer.status != http.StatusOK {
			t.Fatalf("%s answered the machine's signed-in caller %d (%v): %s", pod.instance, answer.status, answer.err, answer.body)
		}
		refused := mpSignedCall(t, pod, caller, notSignedIn, route, body)
		if refused.err != nil || refused.status != http.StatusUnauthorized {
			t.Fatalf("%s answered a caller the broker knows nothing of %d (%v), want 401: %s",
				pod.instance, refused.status, refused.err, refused.body)
		}
	}

	killed := a.kill(t)
	var answered time.Duration
	for deadline := killed.Add(10 * time.Second); ; time.Sleep(mpPoll) {
		answer := mpSignedCall(t, b, caller, signedIn, route, body)
		if answer.err == nil && answer.status == http.StatusOK {
			answered = time.Since(killed)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not answer the machine's signed-in caller within 10 s of the kill: %d (%v) %s\n%s",
				b.instance, answer.status, answer.err, answer.body, mpTail(mpLinesWithout(b.Output.String()), 40))
		}
	}
	refused := mpSignedCall(t, b, caller, notSignedIn, route, body)
	if refused.err != nil || refused.status != http.StatusUnauthorized {
		t.Fatalf("%s answered a caller the broker knows nothing of %d (%v) after the kill, want 401: %s",
			b.instance, refused.status, refused.err, refused.body)
	}
	t.Logf("%s answered the machine's signed-in caller %s after the kill of %s", b.instance, answered.Round(time.Millisecond), a.instance)
}
