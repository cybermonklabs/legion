package ciphertoken_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// approveOnlyToken is the broker's answer for the token its proxy spends for a person's approve
// grant: live, bound to jkt (the broker pod's own key in production), acting as its person, naming
// no project, and holding the one scope approve.
func approveOnlyToken(jkt string, life time.Duration) ciphertoken.Introspection {
	answer := liveToken(jkt, life)
	answer.ProjectScope, answer.Projects, answer.Scope = ciphertoken.ProjectScopePerson, nil, ciphertoken.ScopeApprove
	return answer
}

// TestAnApproveOnlyTokenIsAcceptedOnlyByAVerifierThatServesOne: an approve-only token - the project
// scope person beside the one scope approve, a person, no service and no projects - is a session's
// credential only to a verifier whose service asked for it (Options.ServesApproveOnly), which
// answers its Caller marked approve-only, and is ErrNotASession to every other, so a service that
// does not hold such a token to its approve routes never reads it as its person's ordinary token.
// The rule is exact: approve beside another word, named twice, beside the named scope, with no
// person, with a service beside its person, with a project in its list, with a cut (as_of), or
// under the project scope none or any of the narrowed read words is refused by both verifiers; the
// person-scoped token with no approve is the control both accept unmarked. Each row goes through
// Verify and through Live.
// SessionCaller, the NATS auth service's reading, serves no approve-only token.
func TestAnApproveOnlyTokenIsAcceptedOnlyByAVerifierThatServesOne(t *testing.T) {
	caller := newSigner(t)
	approveOnly := approveOnlyToken(caller.jkt, time.Hour)
	approveCaller := ciphertoken.Caller{Session: "ses_a1b2c3", Person: "sjawhar", ProjectScope: ciphertoken.ProjectScopePerson, ApproveOnly: true}

	approveAndWrite := approveOnlyToken(caller.jkt, time.Hour)
	approveAndWrite.Scope = "approve dispatch.write"
	namedApprove := liveToken(caller.jkt, time.Hour)
	namedApprove.Scope = ciphertoken.ScopeApprove
	noPerson := approveOnlyToken(caller.jkt, time.Hour)
	noPerson.Person, noPerson.Service = "", "example-fleet"
	withService := approveOnlyToken(caller.jkt, time.Hour)
	withService.Service = "example-fleet"
	approveTwice := approveOnlyToken(caller.jkt, time.Hour)
	approveTwice.Scope = ciphertoken.ScopeApprove + " " + ciphertoken.ScopeApprove
	withProject := approveOnlyToken(caller.jkt, time.Hour)
	withProject.Projects = []string{"INFRA"}
	withCut := approveOnlyToken(caller.jkt, time.Hour)
	withCut.AsOf = time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000000Z")
	underWord := func(word string) ciphertoken.Introspection {
		answer := approveOnlyToken(caller.jkt, time.Hour)
		answer.ProjectScope = word
		return answer
	}
	person := approveOnlyToken(caller.jkt, time.Hour)
	person.Scope = "dispatch.write"
	personCaller := ciphertoken.Caller{Session: "ses_a1b2c3", Person: "sjawhar", ProjectScope: ciphertoken.ProjectScopePerson}

	rows := []struct {
		name          string
		answer        ciphertoken.Introspection
		without, with ciphertoken.Caller
	}{
		{name: "the approve-only token", answer: approveOnly, with: approveCaller},
		{name: "approve beside another scope", answer: approveAndWrite},
		{name: "approve beside the named scope", answer: namedApprove},
		{name: "approve naming a service and no person", answer: noPerson},
		{name: "approve naming a service beside its person", answer: withService},
		{name: "approve named twice", answer: approveTwice},
		{name: "approve naming a project", answer: withProject},
		{name: "approve with a cut", answer: withCut},
		{name: "approve under the project scope none", answer: underWord(ciphertoken.ProjectScopeNone)},
		{name: "approve under the project scope public", answer: underWord(ciphertoken.ProjectScopePublic)},
		{name: "approve under read-named", answer: underWord(oauthwire.ProjectScopeReadNamed)},
		{name: "approve under read-person", answer: underWord(oauthwire.ProjectScopeReadPerson)},
		{name: "approve under read-none", answer: underWord(oauthwire.ProjectScopeReadNone)},
		{name: "the person scope with no approve", answer: person, without: personCaller, with: personCaller},
	}
	for _, serves := range []bool{false, true} {
		rig := newRig(t, func(options *ciphertoken.Options) { options.ServesApproveOnly = serves })
		verifier := "without the switch"
		if serves {
			verifier = "with the switch"
		}
		for _, row := range rows {
			t.Run(verifier+"/"+row.name, func(t *testing.T) {
				token := "a-token-of-" + strings.ReplaceAll(row.name, " ", "-")
				rig.broker.Says(token, row.answer)
				want := row.without
				if serves {
					want = row.with
				}
				var wantErr error
				if reflect.DeepEqual(want, ciphertoken.Caller{}) {
					wantErr = ciphertoken.ErrNotASession
				}
				decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
				if !errors.Is(err, wantErr) || !reflect.DeepEqual(decided, want) {
					t.Fatalf("Verify = %+v, %v; want %+v, %v", decided, err, want, wantErr)
				}
				live, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token))
				if !errors.Is(err, wantErr) || !reflect.DeepEqual(live.Caller(), want) {
					t.Fatalf("Live = %+v, %v; want %+v, %v", live.Caller(), err, want, wantErr)
				}
			})
		}
	}
	if _, err := ciphertoken.SessionCaller(approveOnly); !errors.Is(err, ciphertoken.ErrNotASession) {
		t.Fatalf("SessionCaller(the approve-only token) = %v; want ErrNotASession", err)
	}
}
