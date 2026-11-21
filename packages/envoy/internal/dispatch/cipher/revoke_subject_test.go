package cipher

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/wire"
)

// adaReplaced is the revocation of ada's replaced Google account, which another account holds the
// address of now.
var adaReplaced = wire.RevokePersonSubjectRequest{
	Person: "ada@example.com", Issuer: "https://accounts.google.com", Subject: "ada-old", HolderSubject: "ada-new",
}

// RevokePersonSubject is one call: POST /v1/person-subjects/revoke under Dispatch's own token,
// naming the address, the issuer, the replaced account and the holder, and the session generation
// the replacement left when the revocation records one, and the broker's counts back.
func TestRevokePersonSubjectNamesTheReplacedAccountInOneCall(t *testing.T) {
	const named = `{"person":"ada@example.com","issuer":"https://accounts.google.com","subject":"ada-old","holder_subject":"ada-new"`
	atGeneration := adaReplaced
	atGeneration.SessionGeneration = new(int64(5))
	for _, test := range []struct {
		name       string
		revocation wire.RevokePersonSubjectRequest
		want       string
	}{
		{"recording no session generation", adaReplaced, named + `}`},
		{"recording session generation 5", atGeneration, named + `,"session_generation":5}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, captured := serveCapturing(t, http.StatusOK, `{"state":"revoked","signins":1,"machine_logins":2,"tokens_ended":3}`)
			defer server.Close()
			revoked, err := New(server.URL, tokenFile(t, "account-token")).RevokePersonSubject(context.Background(), test.revocation)
			if err != nil {
				t.Fatalf("RevokePersonSubject: %v", err)
			}
			if captured.Method != http.MethodPost || captured.Path != "/v1/person-subjects/revoke" || captured.Auth != "Bearer account-token" {
				t.Fatalf("request = %s %s under %q, want POST /v1/person-subjects/revoke under Bearer account-token", captured.Method, captured.Path, captured.Auth)
			}
			if string(captured.Body) != test.want {
				t.Fatalf("body = %s, want %s", captured.Body, test.want)
			}
			if want := (wire.PersonSubjectRevokedResponse{State: "revoked", SignIns: 1, MachineLogins: 2, TokensEnded: 3}); revoked != want {
				t.Fatalf("answer = %+v, want %+v", revoked, want)
			}
		})
	}
}

// A 200 "superseded" with the generation that superseded it is the broker telling Dispatch that a
// later replacement at the address was applied first, so this revoke changed nothing there: the
// client answers it as a confirmation, with superseded_by, for the caller to settle.
func TestRevokePersonSubjectAcceptsTheBrokersSupersededAnswer(t *testing.T) {
	server, _ := serveCapturing(t, http.StatusOK, `{"state":"superseded","superseded_by":7,"signins":0,"machine_logins":0,"tokens_ended":0}`)
	defer server.Close()
	revocation := adaReplaced
	revocation.SessionGeneration = new(int64(5))
	revoked, err := New(server.URL, tokenFile(t, "account-token")).RevokePersonSubject(context.Background(), revocation)
	if err != nil || revoked.State != "superseded" || revoked.SupersededBy == nil || *revoked.SupersededBy != 7 {
		t.Fatalf("RevokePersonSubject = %+v, %v; want state superseded, superseded_by 7", revoked, err)
	}
}

// Anything but the broker's 200 "revoked", or 200 "superseded" with superseded_by, is an error, so
// the revocation stays owed: a 404, which the broker never answers on this route (a wrong URL, a
// proxy in the way, a broker without the route) and which therefore ended nothing, a 500, and a 200
// that is not the broker's answer.
func TestRevokePersonSubjectFailsOnEverythingButTheBrokersConfirmation(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"404", `{"code":"NOT_FOUND","error":"not found"}`, http.StatusNotFound},
		{"500", `{"code":"INTERNAL","error":"boom"}`, http.StatusInternalServerError},
		{"a 200 with no state", `{"ok":true}`, http.StatusOK},
		{"a 200 superseded with no superseded_by", `{"state":"superseded"}`, http.StatusOK},
		{"a 200 that is no JSON", `<html>default backend</html>`, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, _ := serveCapturing(t, test.status, test.body)
			defer server.Close()
			_, err := New(server.URL, tokenFile(t, "account-token")).RevokePersonSubject(context.Background(), adaReplaced)
			if err == nil || !strings.Contains(err.Error(), "ada@example.com") {
				t.Fatalf("%s: err = %v, want an error naming the address", test.name, err)
			}
			var brokerErr *Error
			if test.status != http.StatusOK && (!errors.As(err, &brokerErr) || brokerErr.Status != test.status) {
				t.Fatalf("%s: err = %v, want the broker's *Error with status %d", test.name, err, test.status)
			}
		})
	}
}
