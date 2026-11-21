//go:build linux

package helper

import (
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnEnrollmentIDThatEchoesItsProofIsKeptAndLoggedNowhere: an answer in the broker's place
// takes a session's enrolment, 201, and names as its enrollment_id the launcher proof the enrolment
// carried. The helper keeps that id nowhere, renews and revokes with it nowhere, and its log holds
// no part of it: Broker.enrollUnder takes the id only as redact.RemoteText.SendBack answers it, and
// an id the rule does not print as it came is a 2xx the helper could not read, as an answer with no
// id is. The session's retry line says so, and the retry enrols it under the id the broker makes.
// Control: that id prints on the "session enrolled" line, so the check sees the member's place.
// Captures helper.answer-words/enrollment-id.
func TestAnEnrollmentIDThatEchoesItsProofIsKeptAndLoggedNowhere(t *testing.T) {
	var out syncBuffer
	r := newLoggedRig(t, "", slog.New(slog.NewJSONHandler(&out, nil)))
	r.login(t)
	r.fake.mu.Lock()
	r.fake.enrollEchoProof = 1
	r.fake.mu.Unlock()
	if reg := r.call(t, Request{Op: "register"}); !reg.OK {
		t.Fatalf("register: %+v", reg)
	}
	enrolled := waitForRecord(t, &out, "session enrolled")
	sess := r.srv.Registry.Get(os.Getpid())
	r.fake.mu.Lock()
	echoed, deletes := slices.Clone(r.fake.echoed), slices.Clone(r.fake.deletes)
	made := r.fake.runtimeIDToID[sess.RuntimeID()]
	r.fake.mu.Unlock()
	if len(echoed) != 1 || len(echoed[0]) < 16 {
		t.Fatalf("control: the stand-in echoed %q; want the one proof the first enrolment carried, so the row is not blind", echoed)
	}
	logged := out.String()
	const capture = "helper.answer-words/enrollment-id"
	redacttest.RequireNone(t, capture, logged)
	if run := proofRun(logged, echoed[0]); run != "" {
		t.Errorf("REDACT-ASSERT %s: the log prints %q, a run of the proof the enrolment carried: %s", capture, run, logged)
	}
	var retried bool
	for _, rec := range logRecords(t, &out) {
		if said, _ := rec["error"].(string); rec["msg"] == "enroll failed; retrying" && strings.Contains(said, "enrollment_id") && strings.Contains(said, "(not printed)") {
			retried = true
		}
	}
	if !retried {
		t.Errorf("no retry line says the broker's enrollment_id is not printed, so the log no longer names the member's place: %s", logged)
	}
	if made == "" || enrolled["enrollment_id"] != made {
		t.Errorf("control: the session enrolled under %v; want %q, the id the broker made on the retry, printed on its line", enrolled["enrollment_id"], made)
	}
	if id := sess.identity().EnrollmentID; id != made {
		t.Errorf("the session keeps the enrollment %q; want %q, never the id that echoes the proof", id, made)
	}
	if len(deletes) != 0 {
		t.Errorf("the helper revoked %q; want no revoke, of the echoed id least of all", deletes)
	}
}

// proofRun is the first run of 8 characters of proof that text holds, and "" for none.
func proofRun(text, proof string) string {
	for at := 0; at+8 <= len(proof); at++ {
		if strings.Contains(text, proof[at:at+8]) {
			return proof[at : at+8]
		}
	}
	return ""
}
