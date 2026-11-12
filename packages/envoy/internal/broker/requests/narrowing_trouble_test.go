// packages/envoy/internal/broker/requests/narrowing_trouble_test.go
// Trouble that is no verdict on a narrowing — a GitHub that cannot be reached or answers no
// verdict, a stored value that cannot be read, the check's own bounds — ends the check and
// leaves the request on the path it took before the check existed: the person is asked, and the
// release makes the refusal the check did not. An outage at GitHub must not stop every GitHub
// App request of the fleet, which is a larger failure than the spent approval the check saves.
//
// Each row below therefore goes the whole way: the request is written pending, a person approves
// it, and the grant is released, which is where the refusal is read. The statuses named are the
// ones the HTTP route maps those errors to (api/handlers_requests.go, grantValues); the rows read
// the error rather than the status because the mapping is already pinned over real HTTP
// (api_test.go's TestAFailedMintIs502MintFailed and api/narrowing_test.go), while which error one
// kind of trouble earns is this file's own subject and the Machine's own answer.
package requests_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// failingSecretRead is the broker's secrets reader with one source's read failing, which is the
// Secrets Manager outage a check and a release both meet: the error is not "no such secret", so
// neither reads it as a secret the policy no longer serves.
type failingSecretRead struct {
	secrets.Reader
	source string
	err    error
}

func (f failingSecretRead) Read(ctx context.Context, source string) (string, error) {
	if source == f.source {
		return "", f.err
	}
	return f.Reader.Read(ctx, source)
}

// storeSourceOf is the Secrets Manager source name the broker reads name's stored value with,
// read out of the policy snapshot it releases from, so a reader that fails this one fails
// exactly the read the check and the release both make.
func storeSourceOf(t *testing.T, f *narrowingFixture, name string) string {
	t.Helper()
	_, source, minted, err := policy.ParseKindSource(f.Policy.Get().Secrets[name].Source)
	if err != nil || !minted {
		t.Fatalf("%s's source is %q, %v; want a minted kind's", name, source, err)
	}
	return source
}

// answerStatus is a token-route hook answering status with body to every token request, for the
// answers GitHub gives that are no verdict on a narrowing.
func answerStatus(status int, header map[string]string, body string) func(http.ResponseWriter, int) bool {
	return func(w http.ResponseWriter, _ int) bool {
		for name, value := range header {
			w.Header().Set(name, value)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return true
	}
}

// TestNarrowingCheckTroubleLetsTheRequestThroughAndTheReleaseRefuses: trouble of any kind leaves
// Create exactly where it was before the check existed — the request is written pending and its
// approver is asked — and the refusal the trouble earns is then read at the release, where it
// always was. No token GitHub would not mint is handed over either way: the release is refused
// too.
func TestNarrowingCheckTroubleLetsTheRequestThroughAndTheReleaseRefuses(t *testing.T) {
	narrowing := &secrets.GitHubNarrowing{Owner: standInOwner, Repositories: []string{standInRepository}}
	for _, tc := range []struct {
		name string
		// trouble arranges it, before the request under test is made.
		trouble func(t *testing.T, f *narrowingFixture)
		// releaseBound bounds the release as the broker bounds every call it serves
		// (requestDeadline, cmd/broker/main.go); zero leaves it the test's own context.
		releaseBound time.Duration
		// want is the refusal the release answers, and nil an error this package refuses with
		// none of its own, which the route answers 500 INTERNAL.
		want error
		// status is what api/handlers_requests.go's grantValues answers that error with, named
		// here so a reader of this row knows what the caller reads.
		status string
	}{
		{
			name: "GitHub cannot be reached at all",
			trouble: func(_ *testing.T, f *narrowingFixture) {
				f.stand.service.Close()
			},
			want:   requests.ErrMintFailed,
			status: "502 MINT_FAILED",
		},
		{
			name: "GitHub answers 500",
			trouble: func(_ *testing.T, f *narrowingFixture) {
				f.stand.answersTokensWith(answerStatus(http.StatusInternalServerError, nil, `{"message":"Server Error"}`))
			},
			want:   requests.ErrMintFailed,
			status: "502 MINT_FAILED",
		},
		{
			name: "the verdict mint answers after the check's own bound",
			trouble: func(_ *testing.T, f *narrowingFixture) {
				f.NarrowingCheckMint = 50 * time.Millisecond
				f.stand.answersTokensWith(func(w http.ResponseWriter, _ int) bool {
					time.Sleep(2 * time.Second)
					w.WriteHeader(http.StatusInternalServerError)
					return true
				})
			},
			releaseBound: 500 * time.Millisecond,
			want:         requests.ErrMintFailed,
			status:       "502 MINT_FAILED",
		},
		{
			name: "GitHub answers the 422 of a rate limit",
			trouble: func(_ *testing.T, f *narrowingFixture) {
				// A 422 carrying GitHub's primary rate-limit header is no verdict on the
				// narrowing, though a 422 is what a refused narrowing reads as
				// (secrets' mintStatusError.rateLimited).
				f.stand.answersTokensWith(answerStatus(http.StatusUnprocessableEntity,
					map[string]string{"X-RateLimit-Remaining": "0"}, `{"message":"API rate limit exceeded"}`))
			},
			want:   requests.ErrMintFailed,
			status: "502 MINT_FAILED",
		},
		{
			name: "the stored value cannot be read",
			trouble: func(t *testing.T, f *narrowingFixture) {
				f.Secrets = failingSecretRead{Reader: f.Secrets, source: storeSourceOf(t, f, "APP_KEY"), err: errors.New("the secrets store could not be read")}
			},
			want:   nil,
			status: "500 INTERNAL",
		},
		{
			name: "the enrollment has spent its checks of the minute",
			trouble: func(t *testing.T, f *narrowingFixture) {
				// The whole burst of refused checks, each of which writes no row and so leaves
				// nothing behind but the one check it spent. The count is the bound itself, so
				// a deployment default raised later spends it here too rather than leaving this
				// row's request with checks to spare.
				f.stand.Answers(airlocktest.GitHubAnswer{Mode: airlocktest.GitHubTokensRefusedRepository, Repository: "missing"})
				spent := &secrets.GitHubNarrowing{Owner: standInOwner, Repositories: []string{"missing"}}
				for i := range requests.DefaultNarrowingChecks.Burst {
					if _, err := f.create(t, spent, "APP_KEY"); !errors.Is(err, secrets.ErrMintNarrowingRefused) {
						t.Fatalf("refused check %d = %v; want the narrowing refused, which writes no row and spends one check", i+1, err)
					}
				}
				// The request under test never reaches GitHub, so the answer in force from here
				// on is the release's alone: the ordinary refusal of every token request.
				f.stand.Answers(airlocktest.GitHubAnswer{Mode: airlocktest.GitHubTokensRefused})
			},
			want:   secrets.ErrMintNarrowingRefused,
			status: "422 NARROWING_REFUSED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := policytest.CaptureLog(t)
			f := newNarrowingFixture(t, map[string]int64{standInOwner: 7})
			tc.trouble(t, f)

			request, err := f.create(t, narrowing, "APP_KEY")
			if err != nil || request.State != "pending" || request.RecordID == nil {
				t.Fatalf("Create(APP_KEY) narrowed while %s = %+v, %v; want it pending, as it was before the check existed", tc.name, request, err)
			}
			if !strings.Contains(logged.String(), requests.NarrowingCheckSkippedMessage) {
				t.Fatalf("no %q line was logged; want the operator told the check was skipped: %s", requests.NarrowingCheckSkippedMessage, logged.String())
			}
			decision, err := f.ApplyDecision(t.Context(), *request.RecordID, true, checkOperator)
			if err != nil || decision.GrantID == "" {
				t.Fatalf("the approver's decision = %+v, %v; want the grant their approval makes", decision, err)
			}

			ctx := t.Context()
			if tc.releaseBound > 0 {
				bounded, cancel := context.WithTimeout(ctx, tc.releaseBound)
				defer cancel()
				ctx = bounded
			}
			release, err := f.Values(ctx, decision.GrantID, f.enrollmentID, narrowing)
			if release.Values != nil {
				t.Fatalf("the release handed over %v; want no value: the narrowing it asked for was never served", release.Values)
			}
			switch {
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("the release answered %v; want %v, which the route answers %s", err, tc.want, tc.status)
			case tc.want == nil && (err == nil || requests.IsRefusal(err)):
				t.Fatalf("the release answered %v; want a failure this package refuses with none of its own, which the route answers %s", err, tc.status)
			}
		})
	}
}

// TestNarrowingCheckTroubleInTheCauseSearchStillRefusesTheRequest: trouble inside the search for
// the part GitHub refused is not the same as trouble reaching a verdict. GitHub has already said
// it will not mint the narrowing, so the request is refused whatever the search finds, and only
// the reason the caller reads is left: a search that runs out of budget or meets an answer that
// is no verdict names the whole narrowing the caller asked for and says the cause was not found.
func TestNarrowingCheckTroubleInTheCauseSearchStillRefusesTheRequest(t *testing.T) {
	narrowing := &secrets.GitHubNarrowing{Owner: standInOwner, Repositories: []string{"missing", standInRepository}}
	for _, tc := range []struct {
		name string
		// refuseEveryToken puts the stand-in on its ordinary refusal instead of installing a
		// hook: the control row, whose search meets no trouble at all.
		refuseEveryToken bool
		// budget bounds the whole check, zero leaving it the default.
		budget time.Duration
		// probe answers every token request after the verdict mint, which the hook below refuses
		// with GitHub's own 422.
		probe func(w http.ResponseWriter) bool
		// mentions is what the refusal the caller reads must say.
		mentions []string
	}{
		{
			name:   "the search runs out of the check's budget",
			budget: 400 * time.Millisecond,
			probe: func(w http.ResponseWriter) bool {
				time.Sleep(2 * time.Second)
				w.WriteHeader(http.StatusUnprocessableEntity)
				return true
			},
			mentions: []string{"the narrowing cannot be served", "APP_KEY", "did not find which part", "owner " + standInOwner, "missing", standInRepository},
		},
		{
			name: "the search meets a rate limit",
			probe: func(w http.ResponseWriter) bool {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
				return true
			},
			mentions: []string{"the narrowing cannot be served", "APP_KEY", "did not find which part", "owner " + standInOwner, "missing", standInRepository},
		},
		{
			name:             "a stand-in that refuses the verdict mint and every probe",
			refuseEveryToken: true,
			mentions:         []string{"the narrowing cannot be served", "APP_KEY", "missing", "does not reach"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNarrowingFixture(t, map[string]int64{standInOwner: 7})
			f.NarrowingCheckBudget = tc.budget
			if tc.refuseEveryToken {
				f.stand.Answers(airlocktest.GitHubAnswer{Mode: airlocktest.GitHubTokensRefused})
			} else {
				// GitHub's own 422 to the verdict mint, and the row's own answer to every probe
				// the search makes after it.
				f.stand.answersTokensWith(func(w http.ResponseWriter, nth int) bool {
					if nth == 1 {
						w.WriteHeader(http.StatusUnprocessableEntity)
						return true
					}
					return tc.probe(w)
				})
			}

			request, err := f.create(t, narrowing, "APP_KEY")
			if !errors.Is(err, secrets.ErrMintNarrowingRefused) || request.ID != "" {
				t.Fatalf("Create(APP_KEY) narrowed to one GitHub refused = %+v, %v; want the narrowing refused", request, err)
			}
			for _, word := range tc.mentions {
				if !strings.Contains(err.Error(), word) {
					t.Errorf("the refusal says %q; want it to name %q", err, word)
				}
			}
			if rows, grants, records := heldRows(t, f.Machine); rows != 0 || grants != 0 || records != 0 {
				t.Fatalf("the refused request left %d request rows, %d grants and %d records; want none of any", rows, grants, records)
			}
		})
	}
}
