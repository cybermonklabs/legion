// resultstoken_test.go drives the verifier with the token a result entry of the broker mints (a
// results token): which verifier serves one, the caller it answers, and each other shape of the
// token it refuses. The signer and the rig are brokertoken_test.go's.
package brokertoken_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
)

// resultsToken is the answer a token a result entry minted gets
// (BROKER_EXCHANGE_RESULT_WORKFLOWS): the one scope results, the project scope results, the
// entry's person and project, the session of its workflow, and the run it was minted for. It is
// the answer the broker's own test holds an exchanged results token to (internal/broker/oauth,
// TestAResultEntrysRunIntrospectsWithItsRun).
func resultsToken(jkt string, life time.Duration) brokertoken.Introspection {
	return brokertoken.Introspection{
		Active:       true,
		Session:      resultsSession,
		Person:       "ada@example.com",
		ProjectScope: brokertoken.ProjectScopeResults,
		Projects:     []string{"ACME"},
		Scope:        brokertoken.ScopeResults,
		CI:           resultsRun(),
		ExpiresAt:    time.Now().Add(life).Unix(),
		ClientID:     brokertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// resultsSession is the session a result entry's token writes under, its workflow's own.
const resultsSession = "ci-results:example-org/example-repo/.github/workflows/ci-results.yaml"

// resultsRun is the run resultsToken carries, a fresh copy each call so a test may spoil one
// field of it.
func resultsRun() *brokertoken.CIRun {
	return &brokertoken.CIRun{
		Repository: "example-org/example-repo", WorkflowRef: "example-org/example-repo/.github/workflows/ci-results.yaml@refs/heads/topic",
		Ref: "refs/heads/topic", SHA: "0123456789abcdef0123456789abcdef01234567", EventName: "push",
		RunID: "7", RunAttempt: "1", RunnerEnvironment: "github-hosted", CheckRunID: "97",
	}
}

// decides is what Verify and Live answer for token on rig, failing the test when either answers
// otherwise than the other.
func decides(t *testing.T, rig *rig, caller *signer, token string) (brokertoken.Caller, error) {
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
func sameCaller(got, want brokertoken.Caller) bool {
	return reflect.DeepEqual(got, want)
}

// TestAResultsTokenIsRefusedWhereNotServed: a results token is no session's credential to a
// verifier whose service did not ask for it, whether or not that verifier serves a read-only
// token, as a build before the switch refused it: it reads the project scope results as a word it
// does not know. The Envoy listener's verifier serves neither token, and Dispatch's serves both
// (cmd/dispatch, brokerTokenVerifier) on the routes its read and results lists name.
func TestAResultsTokenIsRefusedWhereNotServed(t *testing.T) {
	caller := newSigner(t)
	for _, readOnly := range []bool{false, true} {
		rig := newRig(t, func(options *brokertoken.Options) { options.ServesReadOnly = readOnly })
		const token = "a-results-token-where-none-is-served"
		rig.broker.Says(token, resultsToken(caller.jkt, time.Hour))
		if decided, err := decides(t, rig, caller, token); !errors.Is(err, brokertoken.ErrNotASession) || !reflect.DeepEqual(decided, brokertoken.Caller{}) {
			t.Errorf("a verifier serving read-only tokens (%t) and no results token decided %+v, %v; want ErrNotASession and no caller", readOnly, decided, err)
		}
	}
}

// TestAResultsTokenIsServedAsResultsOnly: a verifier whose service asked for results tokens
// (Options.ServesResults) answers one as a Caller marked results-only, naming the session, the
// person, the one project whose results it writes, the project scope results and the run it was
// minted for, and never marked read-only.
func TestAResultsTokenIsServedAsResultsOnly(t *testing.T) {
	rig := newRig(t, func(options *brokertoken.Options) { options.ServesResults = true })
	caller := newSigner(t)
	const token = "a-results-token-where-one-is-served"
	rig.broker.Says(token, resultsToken(caller.jkt, time.Hour))
	decided, err := decides(t, rig, caller, token)
	want := brokertoken.Caller{Session: resultsSession, Person: "ada@example.com", Projects: []string{"ACME"}, ProjectScope: brokertoken.ProjectScopeResults, ResultsOnly: true, CI: resultsRun()}
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
	namedResults.Scope = brokertoken.ScopeResults
	namedResultsAndRun := namedResults
	namedResultsAndRun.CI = resultsRun()
	personResults := liveToken(caller.jkt, time.Hour)
	personResults.ProjectScope, personResults.Projects, personResults.Scope = brokertoken.ProjectScopePerson, nil, "dispatch.write results"
	noneResults := readOnlyToken(caller.jkt, time.Hour)
	noneResults.Scope = brokertoken.ScopeResults
	noScope := resultsToken(caller.jkt, time.Hour)
	noScope.Scope = ""
	readScope := resultsToken(caller.jkt, time.Hour)
	readScope.Scope = brokertoken.ScopeRead
	twoWords := resultsToken(caller.jkt, time.Hour)
	twoWords.Scope = "results machine"
	named := liveToken(caller.jkt, time.Hour)
	named.Person = "ada@example.com"
	namedCaller := brokertoken.Caller{Session: "ses_a1b2c3", Person: "ada@example.com", Projects: []string{"INFRA"}, ProjectScope: brokertoken.ProjectScopeNamed}
	rows := []struct {
		name   string
		answer brokertoken.Introspection
		want   brokertoken.Caller
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
		rig := newRig(t, func(options *brokertoken.Options) {
			options.ServesResults, options.ServesReadOnly = switches.servesResults, switches.readOnly
		})
		for _, row := range rows {
			t.Run(switches.name+"/"+row.name, func(t *testing.T) {
				token := "a-token-of-" + strings.ReplaceAll(switches.name+"-"+row.name, " ", "-")
				rig.broker.Says(token, row.answer)
				var wantErr error
				if reflect.DeepEqual(row.want, brokertoken.Caller{}) {
					wantErr = brokertoken.ErrNotASession
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
// projects, no person, or a service, none of which a result entry mints, and a run beside a
// read-only token.
// The negative control is the whole results token on the same verifier, which it accepts.
func TestAResultsTokenWithoutCIIsRefused(t *testing.T) {
	rig := newRig(t, func(options *brokertoken.Options) { options.ServesResults, options.ServesReadOnly = true, true })
	caller := newSigner(t)
	spoiled := func(change func(*brokertoken.Introspection)) brokertoken.Introspection {
		answer := resultsToken(caller.jkt, time.Hour)
		change(&answer)
		return answer
	}
	for name, answer := range map[string]brokertoken.Introspection{
		"no run":                     spoiled(func(a *brokertoken.Introspection) { a.CI = nil }),
		"a run naming no commit":     spoiled(func(a *brokertoken.Introspection) { a.CI.SHA = "" }),
		"a run naming no check run":  spoiled(func(a *brokertoken.Introspection) { a.CI.CheckRunID = "" }),
		"a run naming no repository": spoiled(func(a *brokertoken.Introspection) { a.CI.Repository = "" }),
		"a run with a short commit":  spoiled(func(a *brokertoken.Introspection) { a.CI.SHA = "0123456789abcdef" }),
		"a run with an upper-case commit": spoiled(func(a *brokertoken.Introspection) {
			a.CI.SHA = "0123456789ABCDEF0123456789ABCDEF01234567"
		}),
		"a run whose check run is no number": spoiled(func(a *brokertoken.Introspection) { a.CI.CheckRunID = "97a" }),
		"a run whose run is zero":            spoiled(func(a *brokertoken.Introspection) { a.CI.RunID = "0" }),
		"a run whose attempt is negative":    spoiled(func(a *brokertoken.Introspection) { a.CI.RunAttempt = "-1" }),
		"a run whose check run is past int64": spoiled(func(a *brokertoken.Introspection) {
			a.CI.CheckRunID = "99999999999999999999"
		}),
		"a run whose workflow_ref does not end in its ref": spoiled(func(a *brokertoken.Introspection) { a.CI.Ref = "refs/heads/other" }),
		"a run whose workflow_ref is not under its repository": spoiled(func(a *brokertoken.Introspection) {
			a.CI.Repository = "example-org/other-repo"
		}),
		"no project":                  spoiled(func(a *brokertoken.Introspection) { a.Projects = nil }),
		"two projects":                spoiled(func(a *brokertoken.Introspection) { a.Projects = []string{"ACME", "INFRA"} }),
		"no person":                   spoiled(func(a *brokertoken.Introspection) { a.Person, a.Service = "", "example-fleet" }),
		"a service beside the person": spoiled(func(a *brokertoken.Introspection) { a.Service = "example-fleet" }),
		"a run beside a read-only token": func() brokertoken.Introspection {
			answer := readOnlyToken(caller.jkt, time.Hour)
			answer.CI = resultsRun()
			return answer
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			token := "a-results-token-with-" + strings.ReplaceAll(name, " ", "-")
			rig.broker.Says(token, answer)
			if decided, err := decides(t, rig, caller, token); !errors.Is(err, brokertoken.ErrNotASession) || !reflect.DeepEqual(decided, brokertoken.Caller{}) {
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
