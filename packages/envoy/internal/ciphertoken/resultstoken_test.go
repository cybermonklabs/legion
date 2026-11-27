// resultstoken_test.go drives the verifier with the token a result entry of the broker mints (a
// results token): which verifier serves one, the caller it answers, and each other shape of the
// token it refuses. The signer and the rig are brokertoken_test.go's.
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

// resultsToken is the answer a results token the broker minted for a result entry's grant gets
// (BROKER_EXCHANGE_RESULT_WORKFLOWS): the one scope results, the project scope results, the
// entry's person and project, the session of its workflow, and the run it was granted for. They
// are the claims the broker's own test holds a result grant to (internal/cipher/oauth,
// TestAResultGrantHoldsItsRun).
func resultsToken(jkt string, life time.Duration) ciphertoken.Introspection {
	return ciphertoken.Introspection{
		Active:       true,
		Session:      resultsSession,
		Person:       "ada@example.com",
		ProjectScope: ciphertoken.ProjectScopeResults,
		Projects:     []string{"ACME"},
		Scope:        ciphertoken.ScopeResults,
		CI:           resultsRun(),
		ExpiresAt:    time.Now().Add(life).Unix(),
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// resultsSession is the session a result entry's token writes under, its workflow's own.
const resultsSession = "ci-results:example-org/example-repo/.github/workflows/ci-results.yaml"

// resultsRun is the run resultsToken carries, a fresh copy each call so a test may spoil one
// field of it.
func resultsRun() *ciphertoken.CIRun {
	return &ciphertoken.CIRun{
		Repository: "example-org/example-repo", WorkflowRef: "example-org/example-repo/.github/workflows/ci-results.yaml@refs/heads/topic",
		Ref: "refs/heads/topic", SHA: "0123456789abcdef0123456789abcdef01234567", EventName: "push",
		RunID: "7", RunAttempt: "1", RunnerEnvironment: "github-hosted", CheckRunID: "97",
	}
}

// decides is what Verify and Live answer for token on rig, failing the test when either answers
// otherwise than the other.
func decides(t *testing.T, rig *rig, caller *signer, token string) (ciphertoken.Caller, error) {
	t.Helper()
	decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
	live, liveErr := rig.verifier.Live(t.Context(), caller.at(resourcePath, token))
	if !errors.Is(liveErr, err) && !errors.Is(err, liveErr) || !sameCaller(live, decided) {
		t.Fatalf("Live = %+v, %v beside Verify's %+v, %v; an open connection is held to the rule the request was", live, liveErr, decided, err)
	}
	return decided, err
}

// sameCaller reports whether got is want, the run and the projects each carries compared by what
// they hold: a Caller holds the run by pointer and the projects as a slice.
func sameCaller(got, want ciphertoken.Caller) bool {
	return reflect.DeepEqual(got, want)
}

// TestAResultsTokenIsRefusedWhereNotServed: a results token is no session's credential to a
// verifier whose service did not ask for it, whether or not that verifier serves a read-only
// token, as a build before the switch refused it: it reads the project scope results as a word it
// does not know. The Envoy listener's verifier serves neither token, and Dispatch's serves both
// (cmd/dispatch, brokerTokenVerifier) on the routes its read and results lists name.
func TestAResultsTokenIsRefusedWhereNotServed(t *testing.T) {
	caller := newSigner(t)
	if decided, err := ciphertoken.SessionCaller(resultsToken(caller.jkt, time.Hour)); !errors.Is(err, ciphertoken.ErrNotASession) || !reflect.DeepEqual(decided, ciphertoken.Caller{}) {
		t.Fatalf("SessionCaller admitted a results token without a route-scoped verifier: %+v, %v", decided, err)
	}
	for _, readOnly := range []bool{false, true} {
		rig := newRig(t, func(options *ciphertoken.Options) { options.ServesReadOnly = readOnly })
		const token = "a-results-token-where-none-is-served"
		rig.broker.Says(token, resultsToken(caller.jkt, time.Hour))
		if decided, err := decides(t, rig, caller, token); !errors.Is(err, ciphertoken.ErrNotASession) || !reflect.DeepEqual(decided, ciphertoken.Caller{}) {
			t.Errorf("a verifier serving read-only tokens (%t) and no results token decided %+v, %v; want ErrNotASession and no caller", readOnly, decided, err)
		}
	}
}

// TestAResultsTokenIsServedAsResultsOnly: a verifier whose service asked for results tokens
// (Options.ServesResults) answers one as a Caller marked results-only, naming the session, the
// person, the one project whose results it writes, the project scope results and the run it was
// minted for, read whole: its workflow's path and its three numbers read out, and never marked
// read-only.
func TestAResultsTokenIsServedAsResultsOnly(t *testing.T) {
	rig := newRig(t, func(options *ciphertoken.Options) { options.ServesResults = true })
	caller := newSigner(t)
	const token = "a-results-token-where-one-is-served"
	rig.broker.Says(token, resultsToken(caller.jkt, time.Hour))
	decided, err := decides(t, rig, caller, token)
	claims := resultsRun()
	run := &oauthwire.Run{
		Repository: claims.Repository, WorkflowRef: claims.WorkflowRef, Ref: claims.Ref, SHA: claims.SHA,
		EventName: claims.EventName, RunnerEnvironment: claims.RunnerEnvironment,
		Workflow: ".github/workflows/ci-results.yaml", ID: 7, Attempt: 1, CheckRun: 97,
	}
	want := ciphertoken.Caller{Session: resultsSession, Person: "ada@example.com", Projects: []string{"ACME"}, ProjectScope: ciphertoken.ProjectScopeResults, ResultsOnly: true, CI: run}
	if err != nil || !sameCaller(decided, want) {
		t.Fatalf("Verify = %+v (run %+v), %v; want %+v (run %+v)", decided, decided.CI, err, want, want.CI)
	}
}

// TestAResultsScopeBesideANamedProjectScopeIsRefused: the scope results goes beside the project
// scope results alone, and the project scope results beside the scope results alone, so every
// other mix is no session's credential to every verifier, the switch or not: results beside
// named, person or none, and the project scope results with no scope, with read, or with a second
// word beside results. A token meant to write only its results would otherwise write its project
// wherever a named token may. The negative control is a named token carrying a scope that is
// neither read nor results, which every verifier accepts unmarked, as it always has.
func TestAResultsScopeBesideANamedProjectScopeIsRefused(t *testing.T) {
	caller := newSigner(t)
	namedResults := liveToken(caller.jkt, time.Hour)
	namedResults.Scope = ciphertoken.ScopeResults
	namedResultsAndRun := namedResults
	namedResultsAndRun.CI = resultsRun()
	personResults := liveToken(caller.jkt, time.Hour)
	personResults.ProjectScope, personResults.Projects, personResults.Scope = ciphertoken.ProjectScopePerson, nil, "dispatch.write results"
	noneResults := readOnlyToken(caller.jkt, time.Hour)
	noneResults.Scope = ciphertoken.ScopeResults
	noScope := resultsToken(caller.jkt, time.Hour)
	noScope.Scope = ""
	readScope := resultsToken(caller.jkt, time.Hour)
	readScope.Scope = ciphertoken.ScopeRead
	twoWords := resultsToken(caller.jkt, time.Hour)
	twoWords.Scope = "results machine"
	named := liveToken(caller.jkt, time.Hour)
	named.Person = "ada@example.com"
	namedCaller := ciphertoken.Caller{Session: "ses_a1b2c3", Person: "ada@example.com", Projects: []string{"INFRA"}, ProjectScope: ciphertoken.ProjectScopeNamed}
	rows := []struct {
		name   string
		answer ciphertoken.Introspection
		want   ciphertoken.Caller
	}{
		{name: "results beside the named scope", answer: namedResults},
		{name: "results and a run beside the named scope", answer: namedResultsAndRun},
		{name: "results among other scopes beside the person scope", answer: personResults},
		{name: "results beside the project scope none", answer: noneResults},
		{name: "the results project scope with no scope", answer: noScope},
		{name: "the results project scope with the scope read", answer: readScope},
		{name: "the results project scope with a second scope", answer: twoWords},
		{name: "the named scope carrying another scope", answer: named, want: namedCaller},
	}
	for _, switches := range []struct {
		name                    string
		servesResults, readOnly bool
	}{{"no switch", false, false}, {"the read-only switch", false, true}, {"the results switch", true, false}, {"both switches", true, true}} {
		rig := newRig(t, func(options *ciphertoken.Options) {
			options.ServesResults, options.ServesReadOnly = switches.servesResults, switches.readOnly
		})
		for _, row := range rows {
			t.Run(switches.name+"/"+row.name, func(t *testing.T) {
				token := "a-token-of-" + strings.ReplaceAll(switches.name+"-"+row.name, " ", "-")
				rig.broker.Says(token, row.answer)
				var wantErr error
				if reflect.DeepEqual(row.want, ciphertoken.Caller{}) {
					wantErr = ciphertoken.ErrNotASession
				}
				if decided, err := decides(t, rig, caller, token); !errors.Is(err, wantErr) || !sameCaller(decided, row.want) {
					t.Fatalf("Verify = %+v, %v; want %+v, %v", decided, err, row.want, wantErr)
				}
			})
		}
	}
}

// TestAResultsTokenWithoutCIIsRefused: a results token carries the run it was minted for, every
// field of it, in the forms the broker mints, since what it may write is held to that run's
// commit and job; one that carries none, a run missing a field, a run whose workflow_ref is not
// its repository, a path and its ref (oauthwire.WorkflowPath), or a run whose commit is not 40
// lowercase hex or whose run, attempt or check run is not a positive decimal, is no session's
// credential even to a verifier that serves results tokens. So is one naming no project, two
// projects, no person, or a service, none of which a result entry mints, and a run beside any
// token but a results token: a read-only token, a job's public and named tokens, and an
// approve-only token.
// The negative control is the whole results token on the same verifier, which it accepts.
func TestAResultsTokenWithoutCIIsRefused(t *testing.T) {
	rig := newRig(t, func(options *ciphertoken.Options) { options.ServesResults, options.ServesReadOnly = true, true })
	caller := newSigner(t)
	spoiled := func(change func(*ciphertoken.Introspection)) ciphertoken.Introspection {
		answer := resultsToken(caller.jkt, time.Hour)
		change(&answer)
		return answer
	}
	for name, answer := range map[string]ciphertoken.Introspection{
		"no run":                     spoiled(func(a *ciphertoken.Introspection) { a.CI = nil }),
		"a run naming no commit":     spoiled(func(a *ciphertoken.Introspection) { a.CI.SHA = "" }),
		"a run naming no check run":  spoiled(func(a *ciphertoken.Introspection) { a.CI.CheckRunID = "" }),
		"a run naming no repository": spoiled(func(a *ciphertoken.Introspection) { a.CI.Repository = "" }),
		"a run with a short commit":  spoiled(func(a *ciphertoken.Introspection) { a.CI.SHA = "0123456789abcdef" }),
		"a run with an upper-case commit": spoiled(func(a *ciphertoken.Introspection) {
			a.CI.SHA = "0123456789ABCDEF0123456789ABCDEF01234567"
		}),
		"a run whose check run is no number": spoiled(func(a *ciphertoken.Introspection) { a.CI.CheckRunID = "97a" }),
		"a run whose run is zero":            spoiled(func(a *ciphertoken.Introspection) { a.CI.RunID = "0" }),
		"a run whose attempt is negative":    spoiled(func(a *ciphertoken.Introspection) { a.CI.RunAttempt = "-1" }),
		"a run whose check run is past int64": spoiled(func(a *ciphertoken.Introspection) {
			a.CI.CheckRunID = "99999999999999999999"
		}),
		"a run whose workflow_ref does not end in its ref": spoiled(func(a *ciphertoken.Introspection) { a.CI.Ref = "refs/heads/other" }),
		"a run whose workflow_ref is not under its repository": spoiled(func(a *ciphertoken.Introspection) {
			a.CI.Repository = "example-org/other-repo"
		}),
		"no project":                  spoiled(func(a *ciphertoken.Introspection) { a.Projects = nil }),
		"two projects":                spoiled(func(a *ciphertoken.Introspection) { a.Projects = []string{"ACME", "INFRA"} }),
		"no person":                   spoiled(func(a *ciphertoken.Introspection) { a.Person, a.Service = "", "example-fleet" }),
		"a service beside the person": spoiled(func(a *ciphertoken.Introspection) { a.Service = "example-fleet" }),
		"a run beside a read-only token": func() ciphertoken.Introspection {
			answer := readOnlyToken(caller.jkt, time.Hour)
			answer.CI = resultsRun()
			return answer
		}(),
		"a run beside a job's public token": func() ciphertoken.Introspection {
			answer := jobToken(caller.jkt, time.Hour, ciphertoken.ProjectScopePublic)
			answer.CI = resultsRun()
			return answer
		}(),
		"a run beside a job's named token": func() ciphertoken.Introspection {
			answer := jobToken(caller.jkt, time.Hour, ciphertoken.ProjectScopeNamed, "ACME")
			answer.CI = resultsRun()
			return answer
		}(),
		"a run beside an approve-only token": func() ciphertoken.Introspection {
			answer := approveOnlyToken(caller.jkt, time.Hour)
			answer.CI = resultsRun()
			return answer
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			token := "a-results-token-with-" + strings.ReplaceAll(name, " ", "-")
			rig.broker.Says(token, answer)
			if decided, err := decides(t, rig, caller, token); !errors.Is(err, ciphertoken.ErrNotASession) || !reflect.DeepEqual(decided, ciphertoken.Caller{}) {
				t.Fatalf("Verify = %+v, %v; want ErrNotASession and no caller", decided, err)
			}
		})
	}
	const whole = "a-whole-results-token"
	rig.broker.Says(whole, resultsToken(caller.jkt, time.Hour))
	if decided, err := decides(t, rig, caller, whole); err != nil || !decided.ResultsOnly {
		t.Fatalf("the whole results token = %+v, %v; want it accepted as results-only", decided, err)
	}
}
