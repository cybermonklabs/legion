package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// urlCredentialToken is the credential these tests plant in a URL, of no real token's shape.
const urlCredentialToken = "plantedUrlCredential7q"

// urlCredentialUserinfo is a userinfo whose password is urlCredentialToken, written apart from any
// URL.
const urlCredentialUserinfo = "user:" + urlCredentialToken

// A death's runtime account is a pod's events and the last lines of its log, and a log line can
// name a URL with a token in its query or its userinfo. The claim keeps the account as its last
// death cause, every other word of it and its last line included, with neither credential: the
// stored claim, the claim's view as the state serves it (ClaimViewOf), and the ended work the
// claim carries into its next process's prompt hold no part of the token.
func TestADeathCauseHoldsNoURLCredentialOfItsLogLines(t *testing.T) {
	h := newHarness(t)
	token, _ := h.launch("LEGION-209", claim.RoleImplementer)
	h.ready(token, "ses_implementer", 1)
	m, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	// A turn was running, so the death ends work the claim records (supervise's recordEnd).
	if err := m.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		t.Fatalf("start the turn: %v", err)
	}
	const detail = "pod legion-legion-209 ended: OOMKilled\n" +
		"fetch https://downloads.example/x?token=" + urlCredentialToken + " failed: 403\n" +
		"push to https://" + urlCredentialUserinfo + "@git.example/acme/widgets refused\n" +
		"the last line names the death"
	const cleaned = "pod legion-legion-209 ended: OOMKilled\n" +
		"fetch https://downloads.example/x failed: 403\n" +
		"push to https://git.example/acme/widgets refused\n" +
		"the last line names the death"
	gone := runtime.Observation{Locator: *m.Claim().Locator, Kind: runtime.Gone, Detail: detail}

	if err := m.Handle(h.ctx, supervise.RuntimeObservation{Observation: gone}); err != nil {
		t.Fatalf("the death of %s: %v", token, err)
	}

	stored := h.stored(token)
	want := string(runtime.Gone) + ": " + cleaned
	if stored.LastDeathCause != want {
		t.Errorf("the stored last death cause is %q, want %q: the account with its URL credentials removed", stored.LastDeathCause, want)
	}
	if wantEnded := "the process died: " + strconv.Quote(want); stored.EndedWork.Cause != wantEnded {
		t.Errorf("the ended work the claim carries is %+v, want its cause %q", stored.EndedWork, wantEnded)
	}
	view := ClaimViewOf(stored)
	if view.LastDeathCause != want {
		t.Errorf("the claim's view carries the last death cause %q, want %q", view.LastDeathCause, want)
	}
	served, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("encode the claim's view: %v", err)
	}
	for what, text := range map[string]string{
		"stored last death cause": stored.LastDeathCause, "ended work": stored.EndedWork.Cause, "claim's view": string(served),
	} {
		if strings.Contains(text, urlCredentialToken) {
			t.Errorf("the %s holds the planted token: %s", what, text)
		}
	}
}

// An error answer can quote the error a far side's read met, and that error names the URL it
// asked: a Dispatch transport error names dispatch_url with its user name, a GitHub one the
// redirect it followed with its query. Both refusal shapes (writeFailure's coded one and
// errorBody's) answer the message with every URL's host and path and every other word kept, and
// no part of the credential.
func TestAnErrorAnswerHoldsNoURLCredentialOfItsMessage(t *testing.T) {
	const message = `Dispatch did not answer the read of handoff record H-7 (Get "https://x/y?token=` + urlCredentialToken + `": EOF), ` +
		"and the listing at https://" + urlCredentialUserinfo + "@dispatch.example/legion failed too; complete again"
	const want = `Dispatch did not answer the read of handoff record H-7 (Get "https://x/y": EOF), ` +
		"and the listing at https://dispatch.example/legion failed too; complete again"
	for _, tc := range []struct {
		name  string
		write func(w http.ResponseWriter)
	}{
		{"a coded refusal", func(w http.ResponseWriter) {
			writeFailure(w, http.StatusBadGateway, "DISPATCH_READ_FAILED", message)
		}},
		{"a claim route's refusal", func(w http.ResponseWriter) {
			writeJSON(w, http.StatusBadGateway, errorBody(message))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tc.write(recorder)
			if strings.Contains(recorder.Body.String(), urlCredentialToken) {
				t.Fatalf("the answer holds the planted token: %s", recorder.Body)
			}
			// Both shapes carry the message under "error"; the coded one carries its code beside it.
			var answer struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
				t.Fatalf("decode the answer %s: %v", recorder.Body, err)
			}
			if answer.Error != want {
				t.Errorf("the answer's error is %q, want %q", answer.Error, want)
			}
		})
	}
}
